import assert from "node:assert/strict";
import { test } from "node:test";

import { libraryName } from "./index.ts";

test("the entry names the library", () => {
  assert.equal(libraryName, "kaiak-control");
});
