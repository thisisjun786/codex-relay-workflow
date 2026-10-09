// deep-check.mjs - self-test of deep.mjs (run by TestRecorderDeepKeepsEveryJSONKey): node deep-check.mjs
import assert from 'node:assert/strict';
import { deep } from './deep.mjs';

const up = s => s.toUpperCase();
const text = x => JSON.stringify(deep(JSON.parse(x), up));
// A "__proto__" key survives with a string value and with an object value, at any level.
assert.equal(text('{"__proto__":"payload","message":"x"}'), '{"__proto__":"PAYLOAD","message":"X"}');
assert.equal(text('{"__proto__":{"a":"b"},"message":"x"}'), '{"__proto__":{"a":"B"},"message":"X"}');
assert.equal(text('{"o":{"__proto__":["p",{"__proto__":"q"}]}}'), '{"o":{"__proto__":["P",{"__proto__":"Q"}]}}');
assert.equal(Object.getPrototypeOf(deep(JSON.parse('{"__proto__":{"a":"b"}}'), up)), Object.prototype);
// Non-string leaves and key order are unchanged.
assert.equal(text('{"b":1,"a":[null,true,2.5,"s"],"c":{}}'), '{"b":1,"a":[null,true,2.5,"S"],"c":{}}');
// A very deep array is walked without a stack overflow, and the string at the bottom is mapped.
let nested = '"leaf"';
for (let i = 0; i < 100000; i++) nested = '[' + nested + ']';
let walk = deep(JSON.parse('{"x":' + nested + '}'), up).x, depth = 0;
while (Array.isArray(walk)) { walk = walk[0]; depth++; }
assert.equal(depth, 100000);
assert.equal(walk, 'LEAF');
console.log('deep: ok');
