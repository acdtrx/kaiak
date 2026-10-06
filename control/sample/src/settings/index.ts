// The sample's process settings, read from the environment. Names and values mirror
// the gateway's where they mean the same thing (KAIAK_CONTROL_TOKEN, KAIAK_LOG_FORMAT),
// so one environment serves both locally.

import path from "node:path";

export type LogFormat = "json" | "text";

export interface ListenAddress {
  host: string;
  // 0 picks a free port.
  port: number;
}

export interface Settings {
  // Absolute path of the config file.
  configFile: string;
  listen: ListenAddress;
  // Ports of the protocol replicas: each another core over the same store, serving
  // only the gateway endpoints on the listen host. Empty for none.
  protocolPorts: number[];
  token: string;
  logFormat: LogFormat;
}

export type Environment = Readonly<Record<string, string | undefined>>;

export const DEFAULT_LISTEN = "127.0.0.1:8090";

// Reads the settings from `env`. A relative config path resolves against INIT_CWD (the
// directory npm was run from, so `npm run dev -w sample` from control/ takes paths as typed)
// or else `cwd`. Throws { code: "settings-invalid" } naming the variable at fault.
export function readSettings(env: Environment, cwd: string): Settings {
  const configFile = env["KAIAK_SAMPLE_CONFIG"];
  if (!configFile) throw invalid("KAIAK_SAMPLE_CONFIG is required: the path of the config file to serve");
  const token = env["KAIAK_CONTROL_TOKEN"];
  if (!token) throw invalid("KAIAK_CONTROL_TOKEN is required: the token gateways present");
  const listenValue = env["KAIAK_SAMPLE_LISTEN"] || DEFAULT_LISTEN;
  const listen = parseListenAddress(listenValue);
  if (!listen) {
    throw invalid(`KAIAK_SAMPLE_LISTEN=${JSON.stringify(listenValue)}: want host:port (port 0 to 65535; [addr]:port for IPv6)`);
  }
  const portsValue = env["KAIAK_SAMPLE_PROTOCOL_PORTS"] || "";
  const protocolPorts = parsePorts(portsValue);
  if (!protocolPorts) {
    throw invalid(`KAIAK_SAMPLE_PROTOCOL_PORTS=${JSON.stringify(portsValue)}: want a comma-separated list of ports (0 to 65535; 0 picks a free one)`);
  }
  const logFormat = env["KAIAK_LOG_FORMAT"] || "json";
  if (logFormat !== "json" && logFormat !== "text") {
    throw invalid(`KAIAK_LOG_FORMAT=${JSON.stringify(logFormat)}: want json or text`);
  }
  return { configFile: path.resolve(env["INIT_CWD"] || cwd, configFile), listen, protocolPorts, token, logFormat };
}

const LISTEN = /^(?:\[([^\]]+)\]|([^:[\]]*)):([0-9]{1,5})$/;

// "host:port", "[ipv6]:port", or ":port" for every IPv4 interface.
function parseListenAddress(value: string): ListenAddress | undefined {
  const match = LISTEN.exec(value);
  if (!match) return undefined;
  const port = Number(match[3]);
  if (port > 65535) return undefined;
  const host = match[1] ?? match[2] ?? "";
  return { host: host === "" ? "0.0.0.0" : host, port };
}

// "8091,8092", or "" for none.
function parsePorts(value: string): number[] | undefined {
  if (value.trim() === "") return [];
  const ports: number[] = [];
  for (const part of value.split(",")) {
    const text = part.trim();
    if (!/^[0-9]{1,5}$/.test(text) || Number(text) > 65535) return undefined;
    ports.push(Number(text));
  }
  return ports;
}

function invalid(message: string): Error & { code: string } {
  return Object.assign(new Error(message), { code: "settings-invalid" });
}
