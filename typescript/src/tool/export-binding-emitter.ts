import { declarationKey, formatInterface } from "../syntax/index.js";
import type { InterfaceFile, TypeExpr } from "../syntax/index.js";
import { interfaceID, interfaceIDHex } from "./interface-id.js";
import type { DiscoveredException, DiscoveredProcedure, SourceDiscovery } from "./source-discovery.js";

export interface ExportProviderBinding {
    readonly localName: string;
    readonly sourceName: string;
    readonly specifier: string;
}

export interface ExportBindingOptions {
    readonly discovery?: SourceDiscovery;
    readonly providers?: ReadonlyMap<string, ExportProviderBinding>;
}

export function emitExportBinding(file: InterfaceFile, options: ExportBindingOptions): string {
    const bytes = interfaceIDHex(interfaceID(formatInterface(file))).match(/../gu)!.map((byte) => `0x${byte}`);
    if (options.discovery === undefined || options.providers === undefined) throw new Error("export binding emission requires discovered providers");
    const codecIndices = codecIndexMap(file);
    const imports = [...new Map([...options.providers.values()].map((provider) => [provider.localName, provider])).values()].sort((left, right) => left.localName < right.localName ? -1 : left.localName > right.localName ? 1 : 0).map((provider) => `import * as ${provider.localName} from ${JSON.stringify(provider.specifier)};`);
    const lines = [
        "import { createExportBindingWithInterfaceID, decodeProgram, decodeProgramsFromPayload, encodeProgram, freezeDispatch } from \"@cerasos/intercall/generated\";",
        ...imports,
        "",
        "const dispatch = freezeDispatch(async (context, procedureKey, payload) => {",
        "    switch (procedureKey) {",
    ];
    for (const procedure of options.discovery.procedures) emitProcedure(lines, procedure, options.providers.get(procedure.wireName)!, codecIndices, options.discovery, file, options.providers);
    lines.push("        default: return { exceptionKey: 0x970e76fcc5e2dacbn, payload: new Uint8Array() };", "    }", "});", `export const exportBinding = createExportBindingWithInterfaceID(dispatch, Uint8Array.from([${bytes.join(", ")}]));`, "");
    return lines.join("\n");
}

function emitProcedure(lines: string[], procedure: DiscoveredProcedure, provider: ExportProviderBinding, codecs: ReadonlyMap<TypeExpr, number>, discovery: SourceDiscovery, file: InterfaceFile, providers: ReadonlyMap<string, ExportProviderBinding>): void {
    const key = declarationKey("procedure", procedure.wireName);
    const canonicalProcedure = file.declarations.find((declaration) => declaration.kind === "procedure-decl" && declaration.name.name === procedure.wireName);
    const parameters = canonicalProcedure?.kind === "procedure-decl" ? canonicalProcedure.params : [];
    lines.push(`        case ${key}n: {`);
    if (parameters.length > 1) {
        const indices = parameters.map((parameter) => `codec${codecs.get(parameter.type as unknown as TypeExpr)!}`).join(", ");
        lines.push("            let values: any[];", "            try {");
        lines.push(`                values = decodeProgramsFromPayload([${indices}], payload);`);
        lines.push("            } catch {", "                return { exceptionKey: 0x3f5fc972f8477b07n, payload: new Uint8Array() };", "            }", "            try {");
        emitProviderCall(lines, provider, parameters.map((_parameter, index) => `values[${index}]`), file, procedure.wireName, codecs);
    } else if (parameters.length === 1) {
        const parameter = parameters[0]!;
        lines.push("            let value: any;", "            try {");
        lines.push(`                value = decodeProgram(codec${codecs.get(parameter.type as unknown as TypeExpr) ?? 0}, payload);`);
        lines.push("            } catch {", "                return { exceptionKey: 0x3f5fc972f8477b07n, payload: new Uint8Array() };", "            }", "            try {");
        emitProviderCall(lines, provider, ["value"], file, procedure.wireName, codecs);
    } else {
        lines.push("            if (payload.byteLength !== 0) return { exceptionKey: 0x3f5fc972f8477b07n, payload: new Uint8Array() };", "            try {");
        emitProviderCall(lines, provider, [], file, procedure.wireName, codecs);
    }
    lines.push("            } catch (error: any) {");
    lines.push("                let matchCount = 0;", "                let matchedKey = 0n;", "                let matchedPayload: Uint8Array = new Uint8Array();", "                try {");
    for (const exception of discovery.exceptions) emitExceptionMatch(lines, exception, providers.get(exception.wireName) ?? provider, codecs, file);
    lines.push("                    if (matchCount === 1) return { exceptionKey: matchedKey, payload: matchedPayload };", "                    return { exceptionKey: 0x1aaec22e85996f50n, payload: new Uint8Array() };", "                } catch {", "                    return { exceptionKey: 0x1aaec22e85996f50n, payload: new Uint8Array() };", "                }", "            }", "        }");
}

function emitProviderCall(lines: string[], provider: ExportProviderBinding, values: readonly string[], file: InterfaceFile, wireName: string, codecs: ReadonlyMap<TypeExpr, number>): void {
    const suffix = values.map((value) => `, ${value}`).join("");
    const procedure = file.declarations.find((declaration) => declaration.kind === "procedure-decl" && declaration.name.name === wireName);
    const result = procedure?.kind === "procedure-decl" ? procedure.result : undefined;
    const encoded = result === undefined ? "new Uint8Array()" : `encodeProgram(codec${codecs.get(result)!}, result)`;
    lines.push(`                const result = await ${provider.localName}[${JSON.stringify(provider.sourceName)}](context${suffix});`);
    lines.push(`                return { exceptionKey: 0n, payload: ${encoded} };`);
}

function emitExceptionMatch(lines: string[], exception: DiscoveredException, provider: ExportProviderBinding, codecs: ReadonlyMap<TypeExpr, number>, file: InterfaceFile): void {
    const key = declarationKey("exception", exception.wireName);
    if (exception.payloadClass) {
        const declaration = file.declarations.find((candidate) => candidate.kind === "exception-decl" && candidate.name.name === exception.wireName);
        const payload = declaration?.kind === "exception-decl" ? declaration.type : undefined;
        lines.push(`                    if (error instanceof ${provider.localName}[${JSON.stringify(exception.sourceName)}]) { matchCount += 1; matchedKey = ${key}n; matchedPayload = ${payload === undefined ? "new Uint8Array()" : `encodeProgram(codec${codecs.get(payload)!}, error.payload)`}; }`);
    }
    else lines.push(`                    if (error === ${provider.localName}[${JSON.stringify(exception.sourceName)}]) { matchCount += 1; matchedKey = ${key}n; }`);
}

function codecIndexMap(file: InterfaceFile): Map<TypeExpr, number> {
    const result = new Map<TypeExpr, number>();
    const add = (type: TypeExpr | undefined) => { if (type !== undefined && !result.has(type)) result.set(type, result.size); };
    for (const declaration of file.declarations) {
        if (declaration.kind === "procedure-decl") {
            for (const parameter of declaration.params) add(parameter.type);
            add(declaration.result);
        } else if (declaration.kind === "exception-decl") add(declaration.type);
    }
    return result;
}
