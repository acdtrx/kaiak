// The keygen command's process entry: prints the new key on stdout, or the error and
// the usage on stderr with exit status 1.

import { USAGE, runKeygen } from "./keygen/index.ts";

const result = runKeygen(process.argv.slice(2));
if (result.ok) {
  process.stdout.write(result.output);
} else {
  process.stderr.write(`keygen: ${result.message}\n${USAGE}\n`);
  process.exitCode = 1;
}
