// r6_seqguard.test.mjs — behavioral unit tests for pure.js `seqGuard`, the
// shared latest-wins guard of the async loaders (R6 B8).
//
// The discipline already lived in app.js as hand-rolled counters
// (`const seq = ++securityReqSeq; … if (seq !== securityReqSeq) return;`),
// and every loader written without it shipped the same bug: two overlapping
// requests, the older response landing last, the screen showing the newer
// header over the older payload. `seqGuard` is that idiom as one
// implementation. This file pins the guard's own contract;
// r6_loaderguard.test.mjs executes the real app.js loaders on top of it.
//
// Responses are landed by the test in the order it chooses, and every
// assertion waits on the loader's own completion promise — ordering comes
// from the promise chain, never from a timer.
import test from 'node:test';
import assert from 'node:assert/strict';
import { seqGuard } from '../assets/pure.js';

// pending is a response the test has not landed yet.
function pending() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// loader is the landing step of an app.js loader and nothing else: take a
// token when the request starts, apply the outcome only if that token is
// still the latest one the guard handed out.
function loader(guard) {
  const applied = [];
  return {
    applied,
    request(outcome) {
      const seq = guard.next();
      return outcome.promise.then(
        (data) => { if (guard.isLatest(seq)) applied.push(data); },
        (err) => { if (guard.isLatest(seq)) applied.push(`err:${err.message}`); },
      );
    },
  };
}

test('seqGuard hands out increasing tokens; only the newest one is latest', () => {
  const g = seqGuard();
  assert.equal(g.next(), 1);
  assert.equal(g.next(), 2);
  assert.equal(g.next(), 3);
  assert.deepEqual([1, 2, 3].map((t) => g.isLatest(t)), [false, false, true]);
});

test('out-of-order landing: the superseded response is dropped, the latest one applied', async () => {
  const g = seqGuard();
  const { applied, request } = loader(g);
  const first = pending();
  const second = pending();
  const p1 = request(first);
  const p2 = request(second);
  second.resolve('key B');
  await p2;
  assert.deepEqual(applied, ['key B']);
  first.resolve('key A');
  await p1;
  assert.deepEqual(applied, ['key B'], 'the older response landing last must not be applied');
});

test('a response is dropped as soon as a newer request has started, even when it lands first', async () => {
  // Landing first would paint a view the user has already navigated away
  // from, only to replace it one round-trip later.
  const g = seqGuard();
  const { applied, request } = loader(g);
  const first = pending();
  const second = pending();
  const p1 = request(first);
  const p2 = request(second);
  first.resolve('key A');
  await p1;
  assert.deepEqual(applied, [], 'a response superseded in flight is stale the moment it lands');
  second.resolve('key B');
  await p2;
  assert.deepEqual(applied, ['key B']);
});

test('in-order, non-overlapping requests all land', async () => {
  const g = seqGuard();
  const { applied, request } = loader(g);
  for (const key of ['key A', 'key B', 'key C']) {
    const outcome = pending();
    const done = request(outcome);
    outcome.resolve(key);
    await done;
  }
  assert.deepEqual(applied, ['key A', 'key B', 'key C']);
});

test('outcome does not matter: a superseded failure is dropped, the latest failure lands', async () => {
  const g = seqGuard();
  const { applied, request } = loader(g);
  const first = pending();
  const second = pending();
  const p1 = request(first);
  const p2 = request(second);
  second.resolve('key B');
  await p2;
  first.reject(new Error('superseded boom'));
  await p1;
  assert.deepEqual(applied, ['key B'], 'a stale failure must not raise an error banner over fresh data');
  const third = pending();
  const p3 = request(third);
  third.reject(new Error('latest boom'));
  await p3;
  assert.deepEqual(applied, ['key B', 'err:latest boom'], 'the latest failure must still be reported');
});

test("guards are independent: one loader's traffic never supersedes another loader's token", async () => {
  const x = loader(seqGuard());
  const y = loader(seqGuard());
  const x1 = pending();
  const x2 = pending();
  const y1 = pending();
  const px1 = x.request(x1); // X token 1
  const py1 = y.request(y1); // Y token 1 — the same number as X's first token
  const px2 = x.request(x2); // X token 2 supersedes X token 1 ONLY
  y1.resolve('Y first');
  await py1;
  assert.deepEqual(y.applied, ['Y first'], "X's newer token must not retire Y's token 1");
  x1.resolve('X first');
  await px1;
  assert.deepEqual(x.applied, [], 'X token 1 is superseded by X token 2');
  x2.resolve('X second');
  await px2;
  assert.deepEqual(x.applied, ['X second']);
  assert.deepEqual(y.applied, ['Y first'], 'X landing must leave the other loader alone');
});

test('every guard instance owns its sequence — there is no module-level counter', () => {
  const a = seqGuard();
  const b = seqGuard();
  a.next();
  a.next();
  assert.equal(b.next(), 1, 'a fresh guard starts its own sequence');
  assert.equal(a.isLatest(1), false);
  assert.equal(b.isLatest(1), true, 'the same numeric token means different requests in different guards');
});
