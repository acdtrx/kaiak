// Boundary lint for control/ (CODING-RULES §7).
//
// A subsystem is a top-level folder under a package's src/. Code outside a subsystem
// imports it only through its entry file (index.ts), and the import graph across all
// files is acyclic. Imports are read from the TypeScript parser's own list of module
// specifiers (static, re-export, dynamic and type imports alike), never by pattern
// matching the source (CODING-RULES §4).
//
// Only relative specifiers are checked: bare specifiers name packages, whose entry is
// fixed by their package.json "exports".

import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

import { isStringLiteralLikeNode } from "typescript/unstable/ast/is";
import { API } from "typescript/unstable/sync";

export type ViolationCode = "deep-import" | "cycle";

export interface Violation {
  code: ViolationCode;
  message: string;
}

export class BoundaryCheckError extends Error {
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

const ENTRY_FILE = "index.ts";

// Checks every .ts file under the given src/ directories, each the src/ of one package.
export function checkBoundaries(srcDirs: readonly string[]): Violation[] {
  const roots = srcDirs.map((dir) => path.resolve(dir));
  const files = roots.flatMap(listSourceFiles);
  const imports = readRelativeImports(files);
  return [...findDeepImports(roots, imports), ...findCycles(imports)];
}

function listSourceFiles(dir: string): string[] {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".ts") && !entry.name.endsWith(".d.ts"))
    .map((entry) => path.join(entry.parentPath, entry.name))
    .sort();
}

// Maps each file to the absolute paths its relative imports point at.
function readRelativeImports(files: readonly string[]): Map<string, string[]> {
  const imports = new Map<string, string[]>();
  if (files.length === 0) return imports;

  const api = new API({ cwd: process.cwd() });
  try {
    const snapshot = api.updateSnapshot({ openFiles: [...files] });
    for (const file of files) {
      const sourceFile = snapshot.getDefaultProjectForFile(file)?.program.getSourceFile(file);
      if (!sourceFile) {
        throw new BoundaryCheckError("parse-failed", `TypeScript did not load ${file}`);
      }
      const targets = sourceFile.imports
        .filter(isStringLiteralLikeNode)
        .map((specifier) => specifier.text)
        .filter((specifier) => specifier.startsWith("./") || specifier.startsWith("../"))
        .map((specifier) => path.resolve(path.dirname(file), specifier));
      imports.set(file, targets);
    }
  } finally {
    api.close();
  }
  return imports;
}

function findDeepImports(roots: readonly string[], imports: Map<string, string[]>): Violation[] {
  const violations: Violation[] = [];
  for (const [file, targets] of imports) {
    const root = roots.find((candidate) => isInside(candidate, file));
    if (!root) continue;
    const fromSubsystem = subsystemOf(root, file);
    for (const target of targets) {
      if (!isInside(root, target)) continue;
      const targetSubsystem = subsystemOf(root, target);
      if (targetSubsystem === undefined || targetSubsystem === fromSubsystem) continue;
      if (path.relative(root, target) === path.join(targetSubsystem, ENTRY_FILE)) continue;
      violations.push({
        code: "deep-import",
        message: `${display(file)} imports ${display(target)}; import subsystem "${targetSubsystem}" through its ${ENTRY_FILE}`,
      });
    }
  }
  return violations;
}

// Reports each strongly connected component of the file graph that holds a cycle
// (Tarjan's algorithm). Edges to files outside the checked set are ignored.
function findCycles(imports: Map<string, string[]>): Violation[] {
  const index = new Map<string, number>();
  const lowLink = new Map<string, number>();
  const onStack = new Set<string>();
  const stack: string[] = [];
  const violations: Violation[] = [];

  const edgesOf = (file: string): string[] => (imports.get(file) ?? []).filter((target) => imports.has(target));

  const visit = (file: string): void => {
    const order = index.size;
    index.set(file, order);
    lowLink.set(file, order);
    stack.push(file);
    onStack.add(file);

    for (const target of edgesOf(file)) {
      if (!index.has(target)) {
        visit(target);
        lowLink.set(file, Math.min(lowLinkOf(file), lowLinkOf(target)));
      } else if (onStack.has(target)) {
        lowLink.set(file, Math.min(lowLinkOf(file), indexOf(target)));
      }
    }

    if (lowLinkOf(file) !== indexOf(file)) return;
    const component: string[] = [];
    let member: string | undefined;
    do {
      member = stack.pop();
      if (member === undefined) throw new BoundaryCheckError("invariant", "Tarjan stack underflow");
      onStack.delete(member);
      component.push(member);
    } while (member !== file);

    const selfImport = component.length === 1 && edgesOf(file).includes(file);
    if (component.length > 1 || selfImport) {
      violations.push({
        code: "cycle",
        message: `import cycle among: ${component.reverse().map(display).join(", ")}`,
      });
    }
  };

  const indexOf = (file: string): number => readNumber(index, file);
  const lowLinkOf = (file: string): number => readNumber(lowLink, file);

  for (const file of imports.keys()) {
    if (!index.has(file)) visit(file);
  }
  return violations;
}

function readNumber(map: Map<string, number>, key: string): number {
  const value = map.get(key);
  if (value === undefined) throw new BoundaryCheckError("invariant", `no graph entry for ${key}`);
  return value;
}

function subsystemOf(root: string, file: string): string | undefined {
  const parts = path.relative(root, file).split(path.sep);
  return parts.length > 1 ? parts[0] : undefined;
}

function isInside(dir: string, file: string): boolean {
  const relative = path.relative(dir, file);
  return relative !== "" && !relative.startsWith("..") && !path.isAbsolute(relative);
}

function display(file: string): string {
  return path.relative(process.cwd(), file);
}

// The src/ directory of every workspace listed in control/package.json.
function workspaceSrcDirs(): string[] {
  const controlDir = path.resolve(import.meta.dirname, "..");
  const manifest: unknown = JSON.parse(readFileSync(path.join(controlDir, "package.json"), "utf8"));
  const workspaces =
    typeof manifest === "object" && manifest !== null && "workspaces" in manifest ? manifest.workspaces : undefined;
  if (!Array.isArray(workspaces) || !workspaces.every((entry) => typeof entry === "string")) {
    throw new BoundaryCheckError("bad-manifest", "control/package.json has no workspaces list");
  }
  return workspaces.map((workspace) => path.join(controlDir, workspace, "src"));
}

if (import.meta.main) {
  const violations = checkBoundaries(workspaceSrcDirs());
  for (const violation of violations) {
    console.error(`${violation.code}: ${violation.message}`);
  }
  if (violations.length > 0) {
    process.exitCode = 1;
  } else {
    console.log("boundaries ok");
  }
}
