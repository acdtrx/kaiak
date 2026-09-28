import assert from "node:assert/strict";
import { describe, test } from "node:test";

import {
  PROTOCOL_VERSION,
  checkBearerToken,
  checkGatewayRequest,
  checkProtocolVersion,
  errorBody,
  readInstance,
} from "./index.ts";
import type { RequestHeaders } from "./index.ts";

const TOKEN = "s3cret-token";

const goodHeaders: RequestHeaders = {
  authorization: `Bearer ${TOKEN}`,
  "kaiak-protocol": "2",
  "kaiak-instance": "gw-1",
};

describe("bearer token", () => {
  test("the right token passes, whatever the scheme's case", () => {
    assert.equal(checkBearerToken(`Bearer ${TOKEN}`, TOKEN), undefined);
    assert.equal(checkBearerToken(`bearer ${TOKEN}`, TOKEN), undefined);
  });

  test("a missing, malformed, wrong or differently sized token is unauthorized", () => {
    const cases: (string | undefined)[] = [
      undefined,
      `Bearer ${TOKEN}, Bearer ${TOKEN}`,
      TOKEN,
      `Basic ${TOKEN}`,
      "Bearer ",
      "Bearer s3cret-tokeN",
      "Bearer s3cret",
      `Bearer ${TOKEN}${TOKEN}`,
      `Bearer ${TOKEN} extra`,
    ];
    for (const header of cases) {
      const error = checkBearerToken(header, TOKEN);
      assert.equal(error?.code, "unauthorized", JSON.stringify(header));
      assert.equal(error?.status, 401);
    }
  });

  test("the error never echoes the token", () => {
    const error = checkBearerToken("Bearer guess-token", TOKEN);
    assert.ok(error && !error.message.includes("guess-token") && !error.message.includes(TOKEN));
  });
});

describe("protocol version", () => {
  test("the current version is 2 and passes", () => {
    assert.equal(PROTOCOL_VERSION, 2);
    assert.equal(checkProtocolVersion("2"), undefined);
  });

  test("another or missing version is a mismatch, a repeated one (joined by Node) included", () => {
    for (const header of ["1", "3", "2.0", "", " 2", undefined, "2, 2"]) {
      const error = checkProtocolVersion(header);
      assert.equal(error?.code, "protocol-version-mismatch", JSON.stringify(header));
      assert.equal(error?.status, 400);
    }
  });
});

describe("instance", () => {
  test("an instance ID in the instance shape is read", () => {
    for (const id of ["gw-1", "kaiak-7d9f.pod_a", "A", "a".repeat(253)]) {
      assert.deepEqual(readInstance(id), { ok: true, instance: id });
    }
  });

  test("a missing or malformed instance is invalid, a repeated one (joined by Node) included", () => {
    for (const header of [undefined, "gw-1, gw-2", "", "-gw", "gw 1", "gw/1", "gw@1", "a".repeat(254)]) {
      const result = readInstance(header);
      assert.ok(!result.ok, JSON.stringify(header));
      assert.equal(result.error.code, "instance-invalid");
      assert.equal(result.error.status, 400);
    }
  });
});

describe("a gateway request", () => {
  test("with every header right, yields the instance", () => {
    assert.deepEqual(checkGatewayRequest(goodHeaders, TOKEN), { ok: true, instance: "gw-1" });
  });

  test("is checked token first, then protocol version, then instance", () => {
    const allWrong = { authorization: "Bearer nope", "kaiak-protocol": "9", "kaiak-instance": "-" };
    const codeOf = (headers: RequestHeaders) => {
      const result = checkGatewayRequest(headers, TOKEN);
      return result.ok ? "ok" : result.error.code;
    };
    assert.equal(codeOf(allWrong), "unauthorized");
    assert.equal(codeOf({ ...allWrong, authorization: goodHeaders["authorization"] }), "protocol-version-mismatch");
    assert.equal(codeOf({ ...goodHeaders, "kaiak-instance": "-" }), "instance-invalid");
  });

  test("a header given as an array (never by Node for these) reads as missing", () => {
    const result = checkGatewayRequest({ ...goodHeaders, "kaiak-instance": ["gw-1", "gw-2"] }, TOKEN);
    assert.ok(!result.ok && result.error.message === "Kaiak-Instance header missing");
  });

  test("a failure becomes the { error, detail } body", () => {
    const result = checkGatewayRequest({}, TOKEN);
    assert.ok(!result.ok);
    assert.deepEqual(errorBody(result.error), { error: "unauthorized", detail: result.error.message });
  });
});
