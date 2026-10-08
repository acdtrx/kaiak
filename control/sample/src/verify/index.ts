// The verify command (docs/specs/BACKEND-VERIFY.md): runs kaiak-control's
// verifyBackend against one backend and prints the report, for an operator about to add
// a backend or a model to the sample's config file. It only reports; it never edits the
// config.
//
// stdout is always the report, one JSON document, so it pipes into jq. What a person
// needs besides it — the metadata to paste, what is left to decide by hand, or why the
// check failed — goes to stderr.

import { parseArgs } from "node:util";

import { BACKEND_TYPES, MODEL_CAPABILITIES, verifyBackend } from "kaiak-control";
import type { BackendReport, BackendType, VerifiedCapabilities, VerifyBackendOptions } from "kaiak-control";

export type VerifyResult =
  // The backend was checked: exit status 0 when report.ok, 1 otherwise.
  | { kind: "report"; report: BackendReport; stdout: string; stderr: string }
  // Nothing was checked: bad arguments, an unset variable, input verifyBackend refused.
  | { kind: "error"; message: string };

export const USAGE = [
  "usage (from control/): npm run verify -w sample -- --base-url <url>",
  "         [--type <type>] [--api-key-env <NAME>] [--model <name>] [--timeout-ms <n>]",
  "  --base-url     the backend's base_url, exactly as config spells it",
  `  --type         the backend's type: ${BACKEND_TYPES.join(", ")} (default openai-compatible)`,
  "  --api-key-env  the environment variable holding the backend's API key (never the key itself)",
  "  --model        a backend-side model name to check and describe",
  "  --timeout-ms   bound on each request (default 5000)",
  "stdout: the report as JSON. stderr: with --model and a passing check, the metadata to",
  "merge into the model's config and what is left to decide by hand; on failure, the reason.",
  "Exit status 0 when the check passes, 1 otherwise.",
].join("\n");

// An environment variable name, as a shell spells one.
const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
const DIGITS = /^[0-9]+$/;

// Runs the command on its arguments (without node and the script path), reading the
// API key from env.
export async function runVerify(args: readonly string[], env: NodeJS.ProcessEnv): Promise<VerifyResult> {
  const parsed = parse(args, env);
  if ("message" in parsed) return { kind: "error", message: parsed.message };

  let report: BackendReport;
  try {
    report = await verifyBackend(parsed);
  } catch (error) {
    if (isInputInvalid(error)) return { kind: "error", message: error.message };
    throw error;
  }
  return { kind: "report", report, stdout: `${JSON.stringify(report, null, 2)}\n`, stderr: explain(report, parsed) };
}

function parse(args: readonly string[], env: NodeJS.ProcessEnv): VerifyBackendOptions | { message: string } {
  let values: Record<string, string | undefined>;
  try {
    ({ values } = parseArgs({
      args: [...args],
      options: {
        "base-url": { type: "string" },
        type: { type: "string" },
        "api-key-env": { type: "string" },
        model: { type: "string" },
        "timeout-ms": { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  const baseUrl = values["base-url"];
  if (!baseUrl) return { message: "--base-url is required: the backend's base_url, as config spells it" };
  // Unchecked here: verifyBackend refuses a type config would not take, before sending
  // anything (verify-input-invalid).
  const options: VerifyBackendOptions = { type: (values.type ?? "openai-compatible") as BackendType, baseUrl };

  const keyEnv = values["api-key-env"];
  if (keyEnv !== undefined) {
    if (!ENV_NAME.test(keyEnv)) return { message: `--api-key-env takes a variable name, not "${keyEnv}"` };
    const credential = env[keyEnv];
    // The value is never echoed: it is the secret.
    if (!credential) return { message: `the environment variable ${keyEnv} is not set or is empty` };
    options.credential = credential;
  }

  if (values.model !== undefined) options.model = values.model;

  const timeout = values["timeout-ms"];
  if (timeout !== undefined) {
    if (!DIGITS.test(timeout)) return { message: `--timeout-ms takes a whole number of milliseconds, not "${timeout}"` };
    options.timeoutMs = Number(timeout);
  }
  return options;
}

function isInputInvalid(error: unknown): error is Error & { code: "verify-input-invalid" } {
  return error instanceof Error && (error as { code?: unknown }).code === "verify-input-invalid";
}

// The lines for a person, on stderr: the failure, or with --model the metadata to
// paste and what the report leaves to decide.
export function explain(report: BackendReport, options: VerifyBackendOptions): string {
  if (!report.ok) {
    const failure = report.failure;
    return failure ? `verify: ${failure.code}: ${failure.message}\n` : "verify: the check failed\n";
  }
  if (report.metadata === undefined) return "";

  const model = report.models.find((entry) => entry.id === options.model);
  const lines = [
    `Merge into the model's "metadata" (reported values only, hints left out):`,
    "",
    `  ${JSON.stringify(report.metadata)}`,
    "",
  ];
  const hints = Object.entries(model?.capabilities ?? {}).filter(
    ([name]) => model?.sources.capabilities?.[name as keyof VerifiedCapabilities]?.hint === true,
  );
  if (hints.length > 0) {
    lines.push(`Hints, to confirm by hand: ${hints.map(([name, value]) => `${name} ${String(value)}`).join(", ")}`);
  }
  const reported = report.metadata.capabilities ?? {};
  const toDecide = [
    ...(report.metadata.context_length === undefined ? ["context_length"] : []),
    ...MODEL_CAPABILITIES.filter((name) => !(name in reported)).map((name) => `capabilities.${name}`),
    "reasoning_efforts (when reasoning is true)",
  ];
  lines.push(`Still to decide by hand: ${toDecide.join(", ")}`, "");
  return lines.join("\n");
}
