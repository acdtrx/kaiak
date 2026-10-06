// Message rules the JSON Schemas cannot express. The rule codes are part of the
// contract (docs/specs/CONTROL-PROTOCOL.md, Messages): the gateway reports the same
// code for the same fixture. Each check runs only on a message that passed its schema.

import { isRealTimestamp } from "../calendar/index.ts";
import { pointer } from "../schemas/index.ts";
import type { ValidationIssue } from "../schemas/index.ts";

import type { GatewayStatus, Totals, UsageBatch, UsageRecord } from "./types.ts";

export type MessageRuleCode =
  | "timestamp-invalid"
  | "totals-window-duplicate"
  | "record-instance-mismatch"
  | "record-id-duplicate";

type Report = (code: MessageRuleCode, path: string, message: string) => void;

function collector(): { issues: ValidationIssue[]; report: Report } {
  const issues: ValidationIssue[] = [];
  return { issues, report: (code, path, message) => issues.push({ code, message: `${path}: ${message}`, path }) };
}

function checkTimestamp(value: string, path: string, report: Report): void {
  if (!isRealTimestamp(value)) report("timestamp-invalid", path, `"${value}" is not a real instant`);
}

export function checkUsageRecord(record: UsageRecord, path = ""): ValidationIssue[] {
  const { issues, report } = collector();
  checkTimestamp(record.gateway_time, pointer(path, "gateway_time"), report);
  return issues;
}

export function checkTotals(totals: Totals, path = ""): ValidationIssue[] {
  const { issues, report } = collector();
  const seen = new Map<string, number>();
  totals.windows.forEach((window, index) => {
    const windowPath = pointer(path, "windows", index);
    checkTimestamp(window.window_start, pointer(windowPath, "window_start"), report);
    // A window belongs to one limit: its group (or global) and type.
    const identity = JSON.stringify([window.group ?? null, window.type]);
    const first = seen.get(identity);
    if (first !== undefined) {
      report("totals-window-duplicate", windowPath, `same limit as ${pointer(path, "windows", first)}`);
      return;
    }
    seen.set(identity, index);
  });
  return issues;
}

export function checkUsageBatch(batch: UsageBatch): ValidationIssue[] {
  const { issues, report } = collector();
  const seen = new Map<string, number>();
  batch.records.forEach((record, index) => {
    const recordPath = pointer("/records", index);
    issues.push(...checkUsageRecord(record, recordPath));
    if (record.gateway_instance !== batch.batch.instance) {
      report(
        "record-instance-mismatch",
        pointer(recordPath, "gateway_instance"),
        `"${record.gateway_instance}" is not the batch's instance "${batch.batch.instance}"`,
      );
    }
    const first = seen.get(record.record_id);
    if (first !== undefined) {
      report("record-id-duplicate", pointer(recordPath, "record_id"), `same record ID as ${pointer("/records", first)}`);
      return;
    }
    seen.set(record.record_id, index);
  });
  return issues;
}

export function checkGatewayStatus(status: GatewayStatus): ValidationIssue[] {
  const { issues, report } = collector();
  checkTimestamp(status.started_at, "/started_at", report);
  for (const [backend, { deployments }] of Object.entries(status.backends)) {
    for (const [model, deployment] of Object.entries(deployments)) {
      if (deployment.opened_at !== undefined) {
        checkTimestamp(deployment.opened_at, pointer("/backends", backend, "deployments", model, "opened_at"), report);
      }
    }
  }
  return issues;
}
