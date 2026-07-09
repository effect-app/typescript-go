# Facade emit — before/after samples

Real `.d.ts` output for each schema-model kind, **stock tsc 6.0.3** vs the **patched** compiler
(effect-app ≥ 4.0.0-beta.280). Generated from the fixture below; long `withConstructorDefault` /
`mapFields` / `copy` machinery is elided as `…` for readability — the point is the **base type**
and the **generated `namespace` / `interface`**.

## Source (fixture)

```ts
import * as S from "effect-app/Schema"

// 1. Opaque<X> — no Encoded namespace
export class OpaqueNoEncoded extends S.Opaque<OpaqueNoEncoded>()(S.Struct({
  name: S.String,
  age: S.Number
})) {}

// 2. Opaque<X, X.Encoded>
export class OpaqueWithEncoded extends S.Opaque<OpaqueWithEncoded, OpaqueWithEncoded.Encoded>()(S.Struct({
  id: S.String,
  count: S.Number
})) {}
export namespace OpaqueWithEncoded {
  export interface Encoded extends S.StructNestedEncoded<typeof OpaqueWithEncoded> {}
}

// 3. Class<X>
export class ClassNoEncoded extends S.Class<ClassNoEncoded>("ClassNoEncoded")({ title: S.String }) {}

// 4. Class<X, X.Encoded>
export class ClassWithEncoded extends S.Class<ClassWithEncoded, ClassWithEncoded.Encoded>("ClassWithEncoded")({ label: S.String }) {}
export namespace ClassWithEncoded {
  export interface Encoded extends S.StructNestedEncoded<typeof ClassWithEncoded> {}
}

// 5. Struct
export const MyStruct = S.Struct({ a: S.String, b: S.Number })
export type MyStruct = typeof MyStruct.Type
```

---

## 1. `Opaque<X>` (no Encoded)

**Stock** — the base carries the full inline `S.Struct<{…}>`; no namespace at all:

```ts
declare const OpaqueNoEncoded_base: S.Opaque<OpaqueNoEncoded, typeof S.ExtendedSchemaNoEncoded, S.Struct<{
    readonly name: S.String;
    readonly age: import("effect/Schema").Number & { withConstructorDefault: …; withDecodingDefaultType: … };
}>, {}> & Omit<S.Struct<{
    readonly name: S.String;
    readonly age: import("effect/Schema").Number & { … };
}>, keyof S.Top>;
export declare class OpaqueNoEncoded extends OpaqueNoEncoded_base {}
```

**Patched** — the `S.Struct<{…}>` is gone; base is the compact facade, and `Encoded`/`Make`/services
are synthesized into a namespace (none existed in source):

```ts
declare const OpaqueNoEncoded_base: S.OpaqueFacade<OpaqueNoEncoded, OpaqueNoEncoded.Encoded, OpaqueNoEncoded.Make, OpaqueNoEncoded.DecodingServices, OpaqueNoEncoded.EncodingServices, {}> & {
    readonly fields: { readonly name: S.String; readonly age: … };  // statics preserved, elided
    mapFields: …; readonly copy: …;
};
export declare class OpaqueNoEncoded extends OpaqueNoEncoded_base {}
export interface OpaqueNoEncoded { readonly name: string; readonly age: number; }
export declare namespace OpaqueNoEncoded {
    interface Encoded { readonly name: string; readonly age: number; }
    interface Make { readonly name: string; readonly age: number; }
    type DecodingServices = never;
    type EncodingServices = never;
}
```

## 2. `Opaque<X, X.Encoded>`

**Stock** — same inline `S.Struct<{…}>` in the base; only the source-written `Encoded extends StructNestedEncoded` namespace:

```ts
declare const OpaqueWithEncoded_base: S.Opaque<OpaqueWithEncoded, OpaqueWithEncoded.Encoded, S.Struct<{
    readonly id: S.String;
    readonly count: import("effect/Schema").Number & { … };
}>, {}> & Omit<S.Struct<{ … }>, keyof S.Top>;
export declare class OpaqueWithEncoded extends OpaqueWithEncoded_base {}
export declare namespace OpaqueWithEncoded {
    interface Encoded extends S.StructNestedEncoded<typeof OpaqueWithEncoded> {}   // conditional, recomputed per consumer
}
```

**Patched** — identical shape to case 1: the supplied `Encoded` is materialized to a literal, and `Make`/services are added:

```ts
declare const OpaqueWithEncoded_base: S.OpaqueFacade<OpaqueWithEncoded, OpaqueWithEncoded.Encoded, OpaqueWithEncoded.Make, OpaqueWithEncoded.DecodingServices, OpaqueWithEncoded.EncodingServices, {}> & { … statics … };
export declare class OpaqueWithEncoded extends OpaqueWithEncoded_base {}
export interface OpaqueWithEncoded { readonly id: string; readonly count: number; }
export declare namespace OpaqueWithEncoded {
    interface Encoded { readonly id: string; readonly count: number; }            // literal, not conditional
    interface Make { readonly id: string; readonly count: number; }
    type DecodingServices = never;
    type EncodingServices = never;
}
```

> `Opaque<X>` and `Opaque<X, X.Encoded>` produce the **same** patched output — the facade synthesizes
> the full namespace either way.

## 3. `Class<X>`

**Stock** — `EnhancedClass` carries the full inline `Struct<{…}>`; no namespace:

```ts
declare const ClassNoEncoded_base: S.EnhancedClass<ClassNoEncoded, import("effect/Schema").Struct<{
    title: S.String;
}>, {}>;
export declare class ClassNoEncoded extends ClassNoEncoded_base {}
```

**Patched** — `S.OpaqueClassFacade` (+ `identifier`/`fields`/… from the effect-app facade & statics), full namespace:

```ts
declare const ClassNoEncoded_base: S.OpaqueClassFacade<ClassNoEncoded, ClassNoEncoded.Encoded, ClassNoEncoded.Make, ClassNoEncoded.DecodingServices, ClassNoEncoded.EncodingServices, {}> & { readonly fields: { title: S.String }; mapFields: …; readonly copy: …; };
export declare class ClassNoEncoded extends ClassNoEncoded_base {}
export interface ClassNoEncoded { readonly title: string; }
export declare namespace ClassNoEncoded {
    interface Encoded { readonly title: string; }
    interface Make { readonly title: string; }
    type DecodingServices = never;
    type EncodingServices = never;
}
```

## 4. `Class<X, X.Encoded>`

**Stock** — `EnhancedClass` with the inline `Struct<{…}>` wrapped to override `Encoded`:

```ts
declare const ClassWithEncoded_base: S.EnhancedClass<ClassWithEncoded, Omit<import("effect/Schema").Struct<{
    label: S.String;
}>, "Encoded"> & { readonly Encoded: ClassWithEncoded.Encoded; }, {}>;
export declare class ClassWithEncoded extends ClassWithEncoded_base {}
export declare namespace ClassWithEncoded {
    interface Encoded extends S.StructNestedEncoded<typeof ClassWithEncoded> {}
}
```

**Patched** — same as case 3 (`OpaqueClassFacade` + materialized namespace):

```ts
declare const ClassWithEncoded_base: S.OpaqueClassFacade<ClassWithEncoded, ClassWithEncoded.Encoded, ClassWithEncoded.Make, ClassWithEncoded.DecodingServices, ClassWithEncoded.EncodingServices, {}> & { … statics … };
export declare class ClassWithEncoded extends ClassWithEncoded_base {}
export interface ClassWithEncoded { readonly label: string; }
export declare namespace ClassWithEncoded {
    interface Encoded { readonly label: string; }
    interface Make { readonly label: string; }
    type DecodingServices = never;
    type EncodingServices = never;
}
```

> Error models (`S.ErrorClass`/`S.TaggedErrorClass`) are identical but emit `S.OpaqueErrorFacadeClass`
> with the `Cause.YieldableError` brand preserved.

## 5. `Struct`

**Stock** — the giant inline `S.Struct<{…}>` is the const's type; only a `type X = typeof X.Type` companion:

```ts
export declare const MyStruct: S.Struct<{
    readonly a: S.String;
    readonly b: import("effect/Schema").Number & { withConstructorDefault: …; withDecodingDefaultType: … };
}>;
export type MyStruct = typeof MyStruct.Type;
```

**Patched** — `S.StructFacade` (a real effect-app `Struct<Fields>`, Workflow-compatible), with `Fields`/`Encoded`/`Make`/services materialized; the `type X` companion is dropped for an `interface X`:

```ts
export declare const MyStruct: S.StructFacade<MyStruct, MyStruct.Encoded, MyStruct.Make, MyStruct.DecodingServices, MyStruct.EncodingServices, MyStruct.Fields>;
export interface MyStruct { readonly a: string; readonly b: number; }
export declare namespace MyStruct {
    interface Fields { readonly a: S.String; readonly b: … }
    interface Encoded { readonly a: string; readonly b: number; }
    interface Make { readonly a: string; readonly b: number; }
    type DecodingServices = never;
    type EncodingServices = never;
}
```

---

The win: stock inlines the full `S.Struct<{…field machinery…}>` in every model's emitted base (and
re-derives `Encoded`/`Type` at each consumer); patched replaces it with a compact facade + named
namespace interfaces materialized **once**, so consumers read `X.Encoded` / `X.Type` by name.
See `effect-schema-facade-emit.md` for the why/how and measured −29.5% instantiation drop.
