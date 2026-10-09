// deep maps fn over every string of a JSON value with an explicit stack, so a value nested at the Node stack edge (the CRW-749 cases) is
// transformed where a recursive walk would overflow before the oracle's own answer does. Every key is kept as an own data property:
// a plain assignment would run the __proto__ setter and drop a "__proto__" key that JSON.parse created (CRW-1029).
const put = (dst, k, x) => { if (Array.isArray(dst)) dst[k] = x; else Object.defineProperty(dst, k, { value: x, enumerable: true, writable: true, configurable: true }); };
export const deep = (v, fn) => {
  if (typeof v === 'string') return fn(v);
  if (!v || typeof v !== 'object') return v;
  const root = Array.isArray(v) ? [] : {};
  const stack = [[v, root]];
  while (stack.length) {
    const [src, dst] = stack.pop();
    for (const [k, x] of Array.isArray(src) ? src.map((e, i) => [i, e]) : Object.entries(src)) {
      if (typeof x === 'string') put(dst, k, fn(x));
      else if (x && typeof x === 'object') { const child = Array.isArray(x) ? [] : {}; put(dst, k, child); stack.push([x, child]); }
      else put(dst, k, x);
    }
  }
  return root;
};
