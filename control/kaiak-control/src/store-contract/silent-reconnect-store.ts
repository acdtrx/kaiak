// A deliberately broken store for the contract tests' negative control: its change
// channel comes back after dropping changes without announcing a catch-up.

import { lossyChannelSubject } from "./lossy-channel.ts";
import { storeContractTests } from "./index.ts";

storeContractTests(lossyChannelSubject("silent reconnect (expected to fail)", "none"));
