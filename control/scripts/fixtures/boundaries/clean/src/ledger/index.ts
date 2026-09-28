import { rate } from "../billing/index.ts";

export const total = (units: number): number => units * rate;
