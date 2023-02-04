import { CodeGenerationConfig, ResolvedWunderGraphConfig } from '../configure';
import path from 'path';
import * as fs from 'fs';
import { Logger } from '../logger';

export interface TemplateOutputFile {
	path: string;
	content: string;
	header?: string;
}

export interface Template {
	generate: (config: CodeGenerationConfig) => Promise<TemplateOutputFile[]>;
	dependencies?: () => Template[];
	precedence?: number;
}

export interface CodeGenConfig {
	basePath: string;
	wunderGraphConfig: ResolvedWunderGraphConfig;
	templates: Template[];
}

export interface CodeGenOutWriter {
	writeFileSync: (path: string, content: string) => void;
}

class FileSystem implements CodeGenOutWriter {
	writeFileSync(path: string, content: string): void {
		ensurePath(path);
		fs.writeFileSync(path, content);
	}
}

const ensurePath = (filePath: string) => {
	const dir = path.dirname(filePath);
	if (!fs.existsSync(dir)) {
		fs.mkdirSync(dir, { recursive: true });
	}
};

export const collectAllTemplates = (templates: Template[], maxTemplateDepth = 25, level = 0) => {
	const allTemplates = new Map<string, Template>();

	if (level > maxTemplateDepth) {
		return allTemplates.values();
	}

	for (const tpl of templates) {
		allTemplates.set(tpl.constructor.name, tpl);
		const deps = tpl?.dependencies?.() || [];
		for (const dep of collectAllTemplates(deps, maxTemplateDepth, level + 1)) {
			allTemplates.set(dep.constructor.name, dep);
		}
	}

	const all = allTemplates.values();
	return Array.from(all).sort((a, b) => {
		return (b.precedence || 0) - (a.precedence || 0);
	});
};

export const doNotEditHeader = '// Code generated by wunderctl. DO NOT EDIT.\n\n';

export const GenerateCode = async (config: CodeGenConfig, customOutWriter?: CodeGenOutWriter) => {
	config.templates = Array.from(collectAllTemplates(config.templates));

	const generateConfig: CodeGenerationConfig = {
		config: config.wunderGraphConfig,
		outPath: config.basePath,
		wunderGraphDir: process.env.WG_DIR_ABS!,
	};

	const outWriter = customOutWriter || new FileSystem();
	const generators: Promise<TemplateOutputFile[]>[] = [];
	config.templates.forEach((template) => {
		generators.push(template.generate(generateConfig));
	});
	const resolved = await Promise.all(generators);
	const rawOutFiles: TemplateOutputFile[] = resolved.reduce((previousValue, currentValue) => [
		...previousValue,
		...currentValue,
	]);
	const outFiles = mergeTemplateOutput(rawOutFiles);
	outFiles.forEach((file) => {
		const content = `${file.header || ''}${file.content}`;
		const outPath = path.join(config.basePath, file.path);
		outWriter.writeFileSync(outPath, content);
		Logger.info(`${outPath} updated`);
	});
};

export const mergeTemplateOutput = (outFiles: TemplateOutputFile[]): TemplateOutputFile[] => {
	const merged: TemplateOutputFile[] = [];
	outFiles.forEach((file) => {
		const existing = merged.find((out) => out.path === file.path);
		if (existing) {
			existing.content += '\n\n' + file.content;
		} else {
			merged.push(file);
		}
	});
	merged.forEach((file) => {
		while (file.content.search('\n\n\n') !== -1) {
			file.content = file.content.replace('\n\n\n', '\n\n');
		}
	});
	return merged;
};
