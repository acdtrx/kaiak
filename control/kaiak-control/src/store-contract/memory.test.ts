// The store contract run against the in-memory store: several cores of one process share
// one store, so the second handle is the store itself.

import { createMemoryStore } from "../storage/index.ts";

import { storeContractTests } from "./index.ts";

storeContractTests({ name: "memory", create: createMemoryStore });
