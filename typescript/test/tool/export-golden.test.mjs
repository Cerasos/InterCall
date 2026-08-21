import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";
import { buildValidatedExportSource, decodeExportArguments, discoverSourceExports, encodeExportResult, loadCompilerProject, normalizeSourceOperands } from "../../dist/tool/index.js";

function build() {
    const project = loadCompilerProject(resolve("test/fixtures/compiler/tsconfig-discovery.json"));
    const operands = normalizeSourceOperands(project, ["test/fixtures/compiler/discovery.ts"]);
    return buildValidatedExportSource(project, discoverSourceExports(project, operands));
}

test("export golden source is deterministic and strictly validated", () => {
    const expected = readFileSync(new URL("../fixtures/export/discovery-export.golden.ts", import.meta.url), "utf8");
    assert.equal(build().generatedSource, expected);
    assert.equal(build().generatedSource, expected);
});

test("multiple parameters are emitted as indexed arguments", () => {
    const project = loadCompilerProject(resolve("test/fixtures/compiler/tsconfig-discovery.json"));
    const operands = normalizeSourceOperands(project, ["test/fixtures/compiler/multiple-params.ts"]);
    const generated = buildValidatedExportSource(project, discoverSourceExports(project, operands)).generatedSource;
    assert.match(generated, /provider_0\["add"\]\(context, values\[0\], values\[1\]\)/);
    assert.doesNotMatch(generated, /provider_0\["add"\]\(context, \.\.\.values\)/);
});

test("direct export helpers return fixed malformed and encoding results", () => {
    assert.deepEqual(decodeExportArguments([], [new Uint8Array()]), { ok: false, exception: "invalid_arguments" });
    assert.deepEqual(encodeExportResult(undefined, undefined), { ok: true, payload: new Uint8Array() });
});
