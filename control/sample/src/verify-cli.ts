// The verify command's process entry: the report on stdout and the notes for a person
// on stderr, exit status 0 when the check passes; the error and the usage on stderr
// with exit status 1 when nothing was checked.

import { USAGE, runVerify } from "./verify/index.ts";

const result = await runVerify(process.argv.slice(2), process.env);
if (result.kind === "report") {
  process.stdout.write(result.stdout);
  process.stderr.write(result.stderr);
  if (!result.report.ok) process.exitCode = 1;
} else {
  process.stderr.write(`verify: ${result.message}\n${USAGE}\n`);
  process.exitCode = 1;
}
