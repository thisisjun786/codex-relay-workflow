
## File format

A file has a header and a body.

```js
import { RECEIPT } from 'crw:lib/schemas'      // header: zero or more static imports ...
import { greeting } from './hello-lib.js'
export const meta = { name: 'hello', phases: ['greet'] }   // ... then exactly one meta
phase('greet')                                  // body: everything after the meta statement
const hi = await agent(greeting('crw'))         // top-level await
return { hi }                                   // top-level return: the run's result
```

- The header is zero or more static `import` statements followed by exactly one `export const meta = <object literal>`.
  An import has one of the forms `import 'm'`, `import d from 'm'`, `import {a, b as c} from 'm'`,
  `import * as ns from 'm'` and `import d, {a} from 'm'`.
- `meta` must evaluate to a JSON-serialisable object. It is evaluated as an expression in a fresh runtime with no host
  globals, so it cannot call `agent` or read `args`, and under the same determinism stubs as the body (below), so
  `Date.now()`, `Math.random()`, `new Date()` and `Date()` in a `meta` are load errors. It is recorded in the run record.
- The body may use top-level `await` and top-level `return`. These are a CRW extension and the script is not standard
  ESM: the body runs as the body of an async function, and the value it returns is the run's result. A script in the
  shape Claude Workflow accepts (a `meta` and a body with top-level await and return, no imports) is a subset of this
  format.
- An `import` or `export` after the header, a dynamic `import()`, `import.meta`, and any export other than `meta` are
  load errors. The body is checked with a lexer that tracks strings, template literals (including their substitutions),
  comments and regular expression literals, so the words inside them are not taken for statements. A property call
  (`o.import(x)`, also with whitespace or a comment between the dot and the name) and a method named `import` or `export`
  (`{ import(x) { ... } }`) are not imports; a method declaration is told from a call by the `{` that follows its
  parameter list on the same line.
- Script errors keep the body's line numbers: the header is replaced by blank lines in the executed text and the
  transform's source map is given to the runtime.
