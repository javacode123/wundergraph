package apihandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/buger/jsonparser"
	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/wundergraph/graphql-go-tools/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/pkg/astparser"
	"github.com/wundergraph/graphql-go-tools/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/pkg/astvalidation"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/plan"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/resolve"

	"github.com/wundergraph/wundergraph/pkg/authentication"
	"github.com/wundergraph/wundergraph/pkg/engineconfigloader"
	"github.com/wundergraph/wundergraph/pkg/hooks"
	"github.com/wundergraph/wundergraph/pkg/inputvariables"
	"github.com/wundergraph/wundergraph/pkg/interpolate"
	"github.com/wundergraph/wundergraph/pkg/logging"
	"github.com/wundergraph/wundergraph/pkg/pool"
	"github.com/wundergraph/wundergraph/pkg/wgpb"
)

type InternalBuilder struct {
	pool             *pool.Pool
	log              *zap.Logger
	loader           *engineconfigloader.EngineConfigLoader
	api              *Api
	planConfig       plan.Configuration
	resolver         *resolve.Resolver
	definition       *ast.Document
	router           *mux.Router
	renameTypeNames  []resolve.RenameTypeName
	middlewareClient *hooks.Client
}

func NewInternalBuilder(pool *pool.Pool, log *zap.Logger, hooksClient *hooks.Client, loader *engineconfigloader.EngineConfigLoader) *InternalBuilder {
	return &InternalBuilder{
		pool:             pool,
		log:              log,
		loader:           loader,
		middlewareClient: hooksClient,
	}
}

func (i *InternalBuilder) BuildAndMountInternalApiHandler(ctx context.Context, router *mux.Router, api *Api) (streamClosers []chan struct{}, err error) {

	if api.EngineConfiguration == nil {
		// engine config is nil, skipping
		return streamClosers, nil
	}
	if api.AuthenticationConfig == nil ||
		api.AuthenticationConfig.Hooks == nil {
		return streamClosers, fmt.Errorf("authentication config missing")
	}

	planConfig, err := i.loader.Load(*api.EngineConfiguration, api.Options.ServerUrl)
	if err != nil {
		return streamClosers, err
	}

	i.api = api
	i.planConfig = *planConfig
	i.resolver = resolve.New(ctx, resolve.NewFetcher(true), true)

	definition, report := astparser.ParseGraphqlDocumentString(api.EngineConfiguration.GraphqlSchema)
	if report.HasErrors() {
		return streamClosers, report
	}
	i.definition = &definition
	err = asttransform.MergeDefinitionWithBaseSchema(i.definition)
	if err != nil {
		return streamClosers, err
	}

	i.log.Debug("configuring API",
		zap.Int("numOfOperations", len(api.Operations)),
	)

	route := router.NewRoute()
	i.router = route.Subrouter()

	// RenameTo is the correct name for the origin
	// for the downstream (client), we have to reverse the __typename fields
	// this is why Types.RenameTo is assigned to rename.From
	for _, configuration := range planConfig.Types {
		i.renameTypeNames = append(i.renameTypeNames, resolve.RenameTypeName{
			From: []byte(configuration.RenameTo),
			To:   []byte(configuration.TypeName),
		})
	}

	for _, operation := range api.Operations {
		err = i.registerOperation(operation)
		if err != nil {
			i.log.Error("registerOperation", zap.Error(err))
		}
	}

	return streamClosers, err
}

func (i *InternalBuilder) registerOperation(operation *wgpb.Operation) error {

	apiPath := operationApiPath(operation.Path)

	if operation.Engine == wgpb.OperationExecutionEngine_ENGINE_NODEJS {
		return i.registerNodeJsOperation(operation, apiPath)
	}

	shared := i.pool.GetShared(context.Background(), i.planConfig, pool.Config{
		RenameTypeNames: i.renameTypeNames,
	})

	shared.Doc.Input.ResetInputString(operation.Content)
	shared.Parser.Parse(shared.Doc, shared.Report)

	if shared.Report.HasErrors() {
		return fmt.Errorf(ErrMsgOperationParseFailed, shared.Report)
	}

	shared.Normalizer.NormalizeNamedOperation(shared.Doc, i.definition, []byte(operation.Name), shared.Report)
	if shared.Report.HasErrors() {
		return fmt.Errorf(ErrMsgOperationNormalizationFailed, shared.Report)
	}

	state := shared.Validation.Validate(shared.Doc, i.definition, shared.Report)
	if state != astvalidation.Valid {
		return fmt.Errorf(ErrMsgOperationValidationFailed, shared.Report)
	}

	preparedPlan := shared.Planner.Plan(shared.Doc, i.definition, operation.Name, shared.Report)
	shared.Postprocess.Process(preparedPlan)

	switch operation.OperationType {
	case wgpb.OperationType_QUERY,
		wgpb.OperationType_MUTATION:
		p, ok := preparedPlan.(*plan.SynchronousResponsePlan)
		if !ok {
			return nil
		}

		extractedVariables := make([]byte, len(shared.Doc.Input.Variables))
		copy(extractedVariables, shared.Doc.Input.Variables)

		handler := &InternalApiHandler{
			preparedPlan:       p,
			operation:          operation,
			extractedVariables: extractedVariables,
			log:                i.log,
			resolver:           i.resolver,
			renameTypeNames:    i.renameTypeNames,
		}

		i.router.Methods(http.MethodPost).Path(apiPath).Handler(handler)
	case wgpb.OperationType_SUBSCRIPTION:
		p, ok := preparedPlan.(*plan.SubscriptionResponsePlan)
		if !ok {
			return nil
		}

		extractedVariables := make([]byte, len(shared.Doc.Input.Variables))
		copy(extractedVariables, shared.Doc.Input.Variables)

		handler := &InternalSubscriptionApiHandler{
			preparedPlan:       p,
			operation:          operation,
			extractedVariables: extractedVariables,
			log:                i.log,
			resolver:           i.resolver,
			renameTypeNames:    i.renameTypeNames,
		}

		i.router.Methods(http.MethodPost).Path(apiPath).Handler(handler)
	}

	return nil
}

func (i *InternalBuilder) registerNodeJsOperation(operation *wgpb.Operation, apiPath string) error {
	variablesValidator, err := inputvariables.NewValidator(cleanupJsonSchema(operation.VariablesSchema), false)
	if err != nil {
		return err
	}

	stringInterpolator, err := interpolate.NewStringInterpolator(cleanupJsonSchema(operation.VariablesSchema))
	if err != nil {
		return err
	}

	route := i.router.Methods(http.MethodPost).Path(apiPath)

	handler := &FunctionsHandler{
		operation:            operation,
		log:                  i.log,
		variablesValidator:   variablesValidator,
		rbacEnforcer:         authentication.NewRBACEnforcer(operation),
		hooksClient:          i.middlewareClient,
		queryParamsAllowList: generateQueryArgumentsAllowList(operation.VariablesSchema),
		stringInterpolator:   stringInterpolator,
		liveQuery: liveQueryConfig{
			enabled:                operation.LiveQueryConfig.Enable,
			pollingIntervalSeconds: operation.LiveQueryConfig.PollingIntervalSeconds,
		},
	}

	route.Handler(handler)
	return nil
}

type InternalApiHandler struct {
	preparedPlan       *plan.SynchronousResponsePlan
	operation          *wgpb.Operation
	extractedVariables []byte
	log                *zap.Logger
	resolver           *resolve.Resolver
	bearerCache        sync.Map
	renameTypeNames    []resolve.RenameTypeName
}

func (h *InternalApiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(logging.RequestIDHeader)
	requestLogger := h.log.With(logging.WithRequestID(reqID))
	r = r.WithContext(context.WithValue(r.Context(), logging.RequestIDKey{}, reqID))

	r = setOperationMetaData(r, h.operation)

	bodyBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(bodyBuf)
	_, err := io.Copy(bodyBuf, r.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	body := bodyBuf.Bytes()

	// internal requests transmit the client request as a JSON object
	// this makes it possible to expose the original client request to hooks triggered by internal requests
	clientRequest, err := NewRequestFromWunderGraphClientRequest(r.Context(), body)
	if err != nil {
		requestLogger.Error("InternalApiHandler.ServeHTTP: Could not create request from __wg.clientRequest",
			zap.Error(err),
			zap.String("url", r.RequestURI),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := pool.GetCtx(r, clientRequest, pool.Config{
		RenameTypeNames: h.renameTypeNames,
	})
	defer pool.PutCtx(ctx)

	variablesBuf, _, _, _ := jsonparser.Get(body, "input")
	if len(variablesBuf) == 0 {
		ctx.Variables = []byte("{}")
	} else {
		ctx.Variables = variablesBuf
	}

	compactBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(compactBuf)
	err = json.Compact(compactBuf, ctx.Variables)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx.Variables = compactBuf.Bytes()

	if len(h.extractedVariables) != 0 {
		ctx.Variables = MergeJsonRightIntoLeft(ctx.Variables, h.extractedVariables)
	}

	ctx.Variables = injectVariables(h.operation, r, ctx.Variables)

	buf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(buf)

	resolveErr := h.resolver.ResolveGraphQLResponse(ctx, h.preparedPlan.Response, nil, buf)
	if done := handleOperationErr(requestLogger, resolveErr, w, "Internal API Handler ResolveGraphQLResponse failed", h.operation); done {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = buf.WriteTo(w)
}

type InternalSubscriptionApiHandler struct {
	preparedPlan       *plan.SubscriptionResponsePlan
	operation          *wgpb.Operation
	extractedVariables []byte
	log                *zap.Logger
	resolver           *resolve.Resolver
	bearerCache        sync.Map
	renameTypeNames    []resolve.RenameTypeName
}

func (h *InternalSubscriptionApiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(logging.RequestIDHeader)
	requestLogger := h.log.With(logging.WithRequestID(reqID))
	r = r.WithContext(context.WithValue(r.Context(), logging.RequestIDKey{}, reqID))

	r = setOperationMetaData(r, h.operation)

	bodyBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(bodyBuf)
	_, err := io.Copy(bodyBuf, r.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	body := bodyBuf.Bytes()

	// internal requests transmit the client request as a JSON object
	// this makes it possible to expose the original client request to hooks triggered by internal requests
	clientRequest, err := NewRequestFromWunderGraphClientRequest(r.Context(), body)
	if err != nil {
		requestLogger.Error("InternalApiHandler.ServeHTTP: Could not create request from __wg.clientRequest",
			zap.Error(err),
			zap.String("url", r.RequestURI),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := pool.GetCtx(r, clientRequest, pool.Config{
		RenameTypeNames: h.renameTypeNames,
	})
	defer pool.PutCtx(ctx)

	variablesBuf, _, _, _ := jsonparser.Get(body, "input")
	if len(variablesBuf) == 0 {
		ctx.Variables = []byte("{}")
	} else {
		ctx.Variables = variablesBuf
	}

	compactBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(compactBuf)
	err = json.Compact(compactBuf, ctx.Variables)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx.Variables = compactBuf.Bytes()

	if len(h.extractedVariables) != 0 {
		ctx.Variables = MergeJsonRightIntoLeft(ctx.Variables, h.extractedVariables)
	}

	ctx.Variables = injectVariables(h.operation, r, ctx.Variables)

	buf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(buf)

	var (
		flushWriter *httpFlushWriter
		ok          bool
	)

	ctx.Context, flushWriter, ok = getFlushWriter(ctx.Context, ctx.Variables, r, w)
	if !ok {
		http.Error(w, "Connection not flushable", http.StatusBadRequest)
		return
	}

	err = h.resolver.ResolveGraphQLSubscription(ctx, h.preparedPlan.Response, flushWriter)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// e.g. client closed connection
			return
		}
		// if the deadline is exceeded (e.g. timeout), we don't have to return an HTTP error
		// we've already flushed a response to the client
		requestLogger.Error("ResolveGraphQLSubscription", zap.Error(err))
		return
	}
}

func NewRequestFromWunderGraphClientRequest(ctx context.Context, body []byte) (*http.Request, error) {
	clientRequest, _, _, _ := jsonparser.Get(body, "__wg", "clientRequest")
	if clientRequest != nil {
		method, err := jsonparser.GetString(body, "__wg", "clientRequest", "method")
		if err != nil {
			return nil, err
		}
		requestURI, err := jsonparser.GetString(body, "__wg", "clientRequest", "requestURI")
		if err != nil {
			return nil, err
		}

		// create a new request from the client request
		// excluding the body because the body is the graphql operation query
		request, err := http.NewRequestWithContext(ctx, method, requestURI, nil)
		if err != nil {
			return nil, err
		}

		err = jsonparser.ObjectEach(body, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
			request.Header.Set(string(key), string(value))
			return nil
		}, "__wg", "clientRequest", "headers")
		if err != nil {
			return nil, err
		}

		return request, nil
	}

	return nil, nil
}
