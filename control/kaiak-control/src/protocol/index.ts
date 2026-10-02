// The checks every gateway request passes before its endpoint runs
// (docs/specs/CONTROL-PROTOCOL.md, Shape and Request checks): bearer token, protocol
// version, instance ID. Framework-agnostic — an HTTP adapter hands in the request's
// headers and turns a failure into its status and error body.

import { createHash, timingSafeEqual } from "node:crypto";

import { definitionChecker } from "../schemas/index.ts";

// The protocol version this library speaks.
export const PROTOCOL_VERSION = 4;

// Header names, lower case as Node presents incoming headers.
export const PROTOCOL_HEADER = "kaiak-protocol";
export const INSTANCE_HEADER = "kaiak-instance";
const AUTHORIZATION_HEADER = "authorization";

export type ProtocolErrorCode = "unauthorized" | "protocol-version-mismatch" | "instance-invalid";

export interface ProtocolError {
  code: ProtocolErrorCode;
  message: string;
  // The HTTP status an adapter answers with.
  status: 400 | 401;
}

// The error body every endpoint uses (CODING-RULES §5): error is the stable code.
export interface ErrorBody {
  error: string;
  detail?: string;
}

// Incoming request headers keyed by lower-case name, as Node's IncomingHttpHeaders.
export type RequestHeaders = Readonly<Record<string, string | string[] | undefined>>;

export type GatewayRequestCheck = { ok: true; instance: string } | { ok: false; error: ProtocolError };

// A gateway request's checks, in order: the token first, so a caller without it
// learns nothing else; then the protocol version; then the instance ID.
export function checkGatewayRequest(headers: RequestHeaders, token: string): GatewayRequestCheck {
  const failure =
    checkBearerToken(single(headers[AUTHORIZATION_HEADER]), token) ??
    checkProtocolVersion(single(headers[PROTOCOL_HEADER]));
  if (failure) return { ok: false, error: failure };
  return readInstance(single(headers[INSTANCE_HEADER]));
}

// Node hands these headers over as one string: a repeated Kaiak-Protocol or
// Kaiak-Instance arrives joined ("1, 1"), which fails its check like any other wrong
// value, and a repeated Authorization keeps only the first. An array (which Node does
// not produce for them) reads as missing.
function single(header: string | string[] | undefined): string | undefined {
  return typeof header === "string" ? header : undefined;
}

export function errorBody(error: ProtocolError): ErrorBody {
  return { error: error.code, detail: error.message };
}

const BEARER = /^Bearer +(\S+) *$/i;

// Returns undefined when the Authorization header carries the token.
export function checkBearerToken(header: string | undefined, token: string): ProtocolError | undefined {
  if (header === undefined) return unauthorized("missing Authorization header");
  const presented = BEARER.exec(header)?.[1];
  if (presented === undefined) return unauthorized("the Authorization header is not a bearer token");
  if (!sameSecret(presented, token)) return unauthorized("wrong token");
  return undefined;
}

// Compares SHA-256 digests in constant time: digests are equal in length whatever the
// inputs, so neither the content nor the length of the token shows in the timing.
function sameSecret(presented: string, expected: string): boolean {
  const digest = (value: string) => createHash("sha256").update(value, "utf8").digest();
  return timingSafeEqual(digest(presented), digest(expected));
}

function unauthorized(message: string): ProtocolError {
  return { code: "unauthorized", message, status: 401 };
}

// Returns undefined when the Kaiak-Protocol header names this library's version.
export function checkProtocolVersion(header: string | undefined): ProtocolError | undefined {
  if (header === String(PROTOCOL_VERSION)) return undefined;
  const received = header === undefined ? "none" : `"${header}"`;
  return {
    code: "protocol-version-mismatch",
    message: `Kaiak-Protocol must be ${PROTOCOL_VERSION}, got ${received}`,
    status: 400,
  };
}

const isInstance = definitionChecker("common.schema.json", "instance");

// Reads the Kaiak-Instance header: the gateway's instance ID, in the instance shape.
export function readInstance(header: string | undefined): GatewayRequestCheck {
  if (header !== undefined && isInstance(header)) return { ok: true, instance: header };
  const reason = header === undefined ? "missing" : "not an instance ID";
  return {
    ok: false,
    error: { code: "instance-invalid", message: `Kaiak-Instance header ${reason}`, status: 400 },
  };
}
