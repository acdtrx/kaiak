import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { schemaDrift, syncSchemas } from "./sync-schemas.ts";

test("kaiak-control's schema copy matches protocol/schema byte for byte", () => {
  assert.deepEqual(schemaDrift(), [], "run `npm run sync-schemas` in control/");
});

test("drift names missing, extra and changed files; a sync clears it", () => {
  const root = mkdtempSync(path.join(tmpdir(), "kaiak-sync-schemas-"));
  try {
    const source = path.join(root, "source");
    const copy = path.join(root, "copy");
    syncSchemas(path.resolve(import.meta.dirname, "../../protocol/schema"), source);
    syncSchemas(source, copy);
    assert.deepEqual(schemaDrift(source, copy), []);

    writeFileSync(path.join(source, "new.schema.json"), "{}");
    writeFileSync(path.join(copy, "stale.schema.json"), "{}");
    writeFileSync(path.join(copy, "common.schema.json"), `${readFileSync(path.join(copy, "common.schema.json"), "utf8")} `);
    assert.deepEqual(schemaDrift(source, copy), ["common.schema.json", "new.schema.json", "stale.schema.json"]);

    syncSchemas(source, copy);
    assert.deepEqual(schemaDrift(source, copy), []);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
