
## How a script is loaded

goja, the JavaScript runtime (a pinned commit; direct dependency), has no modules. The loader (`script.Load`) therefore
builds an executable script in four steps:

1. A header lexer finds the end of the imports and of the `meta` object literal.
2. `meta` is evaluated in a fresh runtime with no host globals, under the determinism stubs, and recorded as JSON.
3. Each imported helper is bundled by esbuild's Go API (a pinned release; direct dependency) with `Bundle`, `Format` IIFE,
   `GlobalName` `__crw_lib_<n>` and `Target` ES2017. The resolver plugin admits only the embedded `crw:lib` files and the
   script's workflows directory.
4. The body is wrapped as `(async function __crw_main(){ const {a, b:c} = __crw_lib_<n>; ...body... })`, syntax-checked and
   lowered with esbuild `Transform` (target ES2017), compiled by goja and called.

goja at the pinned commit runs ES2017 async functions, so the target is ES2017 and `async`/`await` are not lowered to
generators (`TestLoaderRunsMetaHelperAwaitReturn` runs `export const meta`, a `crw:lib` import, a relative import,
top-level await and top-level return in one file against that runtime). If a later pin stops passing that test, the
target drops to ES2016 (esbuild then lowers async/await to generators) and this paragraph says so.
