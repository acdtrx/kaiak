// The store contract run against the in-memory store behind a change channel that drops
// changes while it is down: the catch-up test runs here, as it must for any store whose
// channel can drop a change.

import { lossyChannelSubject } from "./lossy-channel.ts";
import { storeContractTests } from "./index.ts";

storeContractTests(lossyChannelSubject("memory, lossy channel"));
