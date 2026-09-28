import { rate } from "../billing/rates.ts";

export const total = (units: number): number => units * rate;
