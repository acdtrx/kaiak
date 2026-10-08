import assert from "node:assert/strict";
import { test } from "node:test";

import { createListeners } from "./index.ts";

test("every listener hears every value, in subscription order", () => {
  const listeners = createListeners<number>(() => assert.fail("no listener throws"));
  const heard: string[] = [];
  listeners.add((value) => heard.push(`a${value}`));
  listeners.add((value) => heard.push(`b${value}`));
  listeners.emit(1);
  listeners.emit(2);
  assert.deepEqual(heard, ["a1", "b1", "a2", "b2"]);
});

test("the same function subscribed twice is two subscriptions, each unsubscribed on its own", () => {
  const listeners = createListeners<number>(() => assert.fail("no listener throws"));
  let calls = 0;
  const listener = (): void => {
    calls += 1;
  };
  const first = listeners.add(listener);
  listeners.add(listener);
  listeners.emit(0);
  assert.equal(calls, 2);
  first();
  listeners.emit(0);
  assert.equal(calls, 3);
});

test("unsubscribing is safe more than once and leaves the other subscriptions", () => {
  const listeners = createListeners<number>(() => assert.fail("no listener throws"));
  const heard: string[] = [];
  const unsubscribe = listeners.add(() => heard.push("gone"));
  listeners.add(() => heard.push("kept"));
  unsubscribe();
  unsubscribe();
  listeners.emit(0);
  assert.deepEqual(heard, ["kept"]);
});

test("an emit calls the listeners subscribed when it began", () => {
  const listeners = createListeners<number>(() => assert.fail("no listener throws"));
  const heard: string[] = [];
  let unsubscribeLater = (): void => {};
  listeners.add((value) => {
    heard.push(`first${value}`);
    listeners.add((later) => heard.push(`added${later}`));
    unsubscribeLater();
  });
  unsubscribeLater = listeners.add((value) => heard.push(`removed${value}`));
  listeners.emit(1);
  assert.deepEqual(heard, ["first1", "removed1"]);
  heard.length = 0;
  listeners.emit(2);
  assert.deepEqual(heard, ["first2", "added2"]);
});

test("a listener that throws goes to onError with the value; the others are still called", () => {
  const failures: [unknown, number][] = [];
  const listeners = createListeners<number>((error, value) => failures.push([error, value]));
  const broken = new Error("broken");
  const heard: number[] = [];
  listeners.add(() => {
    throw broken;
  });
  listeners.add((value) => heard.push(value));
  listeners.emit(7);
  assert.deepEqual(failures, [[broken, 7]]);
  assert.deepEqual(heard, [7]);
});
