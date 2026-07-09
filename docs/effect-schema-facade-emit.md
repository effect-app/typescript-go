# Effect schema facade emit (`.d.ts` declaration step) — Go

Fork of `microsoft/typescript-go` that expands effect-app schema models into compact, named
**facades** when it writes declaration files — the Go mirror of the same change in
`effect-app/TypeScript`. Source files stay clean; each model is expanded **once** in the emitted
`.d.ts`, so downstream consumers read cheap named interfaces instead of re-instantiating schema
generics. Also re-expressed as `_patches/029-031` on **effect-app/tsgo** (Effect LSP hooks + this).

---

## What

For every effect-app schema model the declaration emitter rewrites the emitted type and generates
a companion namespace:

- **Class / error models** (`S.Class`, `S.TaggedClass`, `S.ErrorClass`, `S.TaggedErrorClass`):
  the hoisted `X_base` const's type is rewritten from `S.EnhancedClass<X, S.Struct<{…full…}>, Inherited>`
  to the matching facade, plus a generated `namespace X` + `Type` `interface X`.
- **Opaque models / requests** (`S.Opaque<X>`, `Req.Query`/`Req.Command`): via `OpaqueFacade`.
- **Struct models** (`S.Struct`, `S.TaggedStruct` — top-level `const`): the inline `S.Struct<{…}>`
  is replaced by `S.StructFacade<…>`, with a sibling `interface X` (decoded `Self`) and a
  **type-only** `declare namespace X` (`Fields`/`Encoded`/`Make`/services) that merges with the
  `const`. The source `export type X = typeof X.Type` companion is dropped.

Constructor → facade mapping:

| source constructor | emitted base | brand (6th arg) |
|---|---|---|
| `S.Class` / `S.TaggedClass` | `S.OpaqueClassFacade` | `{}` |
| `S.ErrorClass` / `S.TaggedErrorClass` | `S.OpaqueErrorFacadeClass` | `Cause.YieldableError` |
| `S.Opaque` / `S.OpaqueFacade` / requests | `S.OpaqueFacade` | `{}` |
| `S.Struct` / `S.TaggedStruct` | `S.StructFacade` | — (6th arg is `X.Fields`) |

Facade types live in **effect-app** (`>= 4.0.0-beta.279`). Exact interfaces (effect-app/libs `@`f74ba9e):

- [`OpaqueFacade`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema/Class.ts#L471)
- [`OpaqueClassFacade`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema/Class.ts#L502)
- [`OpaqueErrorFacadeClass`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema/Class.ts#L580)
- [`StructFacade`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema/Class.ts#L654)
- [`EnhancedClass`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema/Class.ts#L17) (stock class base we rewrite away)
- [effect-app `Struct`](https://github.com/effect-app/libs/blob/f74ba9e6b6805010ec2ff54dd7db119209ea5521/packages/effect-app/src/Schema.ts#L204) (base `StructFacade` extends — effect-app's own, not effect core's)

Activation guard: the transform only runs for source files whose nearest `package.json` references
`effect-app` or an `@effect-app/*` package. Projects that only use `effect` keep stock declaration
emit and never reference effect-app-only `S.*Facade` names. It can also be disabled explicitly with
`--disableEffectAppDtsFacades` or `"disableEffectAppDtsFacades": true`.

---

## Why

Effect schemas encode their decoded/encoded/make/services views as type-level fields computed from
`fields`. Without expansion every consumer re-instantiates those generics (and risks depth-limit
blowups inferring `any`/`unknown`). The win is two parts: **named view interfaces** (~24%) and the
**class-base facade** that drops `S.Struct<{…full…}>` (~76%). Doing both at `.d.ts` emit makes
codegen purely text-based, keeps source clean, and expands each model once.

The facade pins the whole `S.Bottom` surface, so consumers never re-derive any Bottom field from
`fields`.

**Why it compounds:** TypeScript's instantiation cache is **per `Program`**, not global. Each
project reference is its own `Program` and re-instantiates every schema generic it touches from the
dependency `.d.ts`; each parallel checker has its own cache (tsgo multi ≈ 2.2× single); editor /
`tsc` / build / test configs are each another program. So a stock model (inline `S.Struct<{…full…}>`
+ conditional `Encoded`/`Type`) is re-derived **once per program** — N programs = N re-derivations.
The facade materializes the named interfaces **once** at emit, so every program reads cheap literals.
The saving therefore **scales with the number of programs / project references / parallel checkers**;
a many-reference monorepo is the worst case for stock and the best case for the facade.

**Not just performance — correctness.** When the schema generics get deep enough the checker hits its
instantiation/depth limits and **silently** infers `unknown` / `any` for whole views (services,
constructor / `make` members, `Type` / `Encoded`) — no error, the program type-checks green against a
degraded type. It is **non-deterministic** and shows up most under **tsgo's default multi-threaded
mode** (each worker hits the wall independently). Materializing the views as named literals once at
emit makes them fully resolved and stable for every consumer; adopting it surfaced **several real
bugs** in our codebase that the silent `any`/`unknown` had masked. So this removes a class of silent,
non-deterministic type degradations on top of the instantiation cut.

---

## How

Three files (kept minimal so the diff re-applies as `_patches/` on Effect-TS/tsgo):

- `internal/printer/emitresolver.go` — add `CreateTypeOfStructSchemaProperty` to the `EmitResolver`
  interface.
- `internal/checker/emitresolver.go` — implement it (reads a property off the struct const's
  initializer type and serializes the resolved type via `NodeBuilder.TypeToTypeNode(… |
  FlagsUseFullyQualifiedType | FlagsMultilineObjectLiterals)`), factored to share
  `getTypeOfSchemaExpressionProperty` with the class path.
- `internal/transformers/declarations/transform.go` — the transform
  (`createEffectSchemaSourceFileDeclarations` + helpers).

Key mechanics (identical semantics to the TS fork):

- **Materialize from source, serialize the resolved type** — keeps `never` services as `never`,
  avoids checker flow/position crashes from synthesized references.
- **`identifier` is not compiler-emitted** — it's on the class facade interfaces in effect-app
  (`OpaqueClassFacade`/`OpaqueErrorFacadeClass`), not `OpaqueFacade`. The compiler emits only
  per-model precise statics (`fields`/`mapFields`/`to`/`from`/`copy`).
- **Struct stays a `const`** — a type-only `declare namespace X` merges with it; no fake class.
- **`StructFacade` is Workflow-compatible** — `Omit<effect-app Struct<Fields>, …> & { pinned members }`,
  still a real effect-app struct schema (`Workflow.AnyStructSchema`, `Struct<Fields & Context>`,
  `Union`, `.fields.x` keep working).

### Gotchas
- `FlagsUseFullyQualifiedType` is mandatory (nested model refs otherwise collapse to bare sibling names).
- Don't materialize via a type-literal builder (coerces `never`→`{}`); serialize the resolved type.
- `StructFacade` must extend effect-app's `Struct`, not `effect/Schema`'s (distinct types).

---

## Results

Parity with the TS fork on the scanner `api` tree: **0 errors**, byte-identical facade emit, and the
same 112 models faceted (**56 `StructFacade` + 46 `OpaqueErrorFacadeClass` + 10 `OpaqueClassFacade`**).
TS-fork numbers (same source, stock tsc 6.0.3 vs patched): type instantiations 15,704,767 →
11,077,561 (**−29.5%**), check time 21.50s → 17.71s (−17.6%).

**effect-app/tsgo** (`_patches/029-031`): patches apply cleanly in order after Effect's `001-028`
(same pinned submodule `dc37b5249`, no file overlap); the patched binary runs the scanner with
**0 errors + Effect LSP diagnostics (hooks live) + our facades (emitter live)** — the two
enhancements compose.

---

## Related

- [effect-app/typescript-go#2](https://github.com/effect-app/typescript-go/pull/2) (this fork) — tsc original [effect-app/TypeScript#3](https://github.com/effect-app/TypeScript/pull/3); patch form [effect-app/tsgo#1](https://github.com/effect-app/tsgo/pull/1).
- [effect-app/libs#801](https://github.com/effect-app/libs/pull/801) (facade `identifier` + `StructFacade`) + the `StructFacade` effect-app-`Struct` fix on [`main`](https://github.com/effect-app/libs/commits/main).
- Full plan / measurements / fork-layering: scanner [macs-holding/scanner#1597](https://github.com/macs-holding/scanner/pull/1597) (`docs/planning/dts-emit-schema-codegen.md`).
