// A deliberately broken store for the contract tests' negative control: after a
// reconnect its change channel announces the catch-up to its first subscriber only.

import { lossyChannelSubject } from "./lossy-channel.ts";
import { storeContractTests } from "./index.ts";

storeContractTests(lossyChannelSubject("catch-up to one subscriber (expected to fail)", "first-subscriber"));
