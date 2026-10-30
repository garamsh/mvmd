# Layered Architecture

How a system partitioned into horizontal layers is shaped: what a layer is, what may call what, and when a new layer is earned.

## What it is

The system is divided into horizontal layers, each owning one technical responsibility — commonly presentation, business logic, persistence, and the data store beneath them. A layer is a folder, and every file belongs to exactly one.

Nothing here states how a layer is divided inside, or how the system is deployed. The first divides by stack or by subject matter; the second is a separate decision, and a layered system is not required to ship as one unit.

## When it fits

- The system serves one subject area, or few enough that separating them buys nothing yet.
- People are organised by technical skill — interface, service, data — so each layer has an owner.
- The subject matter is not yet understood well enough to partition by it, and restructuring later is acceptable.
- Technical concerns change independently: replacing the data store or the interface should not disturb the rest.

## Boundaries and dependency direction

- **A layer is the unit.** One folder per layer, named for its technical responsibility and never for a feature.
- **Dependencies point down.** A layer references the layers below it and never the one above. Upward flow travels as a return value, or through a contract the lower layer declares and something above satisfies.
- **A layer is reached only through what it declares public.** Reaching past that surface is a defect, even where the language permits it.
- **Whether a layer may call past the layer directly beneath it is contested.** *Pattern-Oriented Software Architecture* vol. 1 (1996) gives strict and relaxed layering as equal variants of its Layers pattern: under the first, a layer calls only the layer directly beneath it; under the second, any layer below. A project takes one, records which, and a call that skips a layer it did not open is a defect.
- **A cycle between layers means the split is wrong**, and so does a file that belongs in two of them.
- **A new layer is earned** by a technical responsibility with a surface that stays stable while its inside changes, which the layer above depends on and which does not depend back. Growth in subject matter earns a boundary instead, and a system that keeps earning those has outgrown this structure.

## What it costs

- A feature is spread across every layer, so no folder holds one whole and one change touches all of them.
- A layer has no internal limit. Unrelated subject areas accumulate side by side inside it as the system grows.
- Testing a layer means substituting the layer below it.
- Layers that only forward calls still have to be written, read, and changed — the architecture sinkhole.

## Signs the choice was wrong

- Most changes touch every layer.
- A large share of a layer's code only forwards calls downward.
- Skipping a layer keeps being proposed, or a lower layer reaches upward for context it was not given.
- Code cannot be found by responsibility, because one layer now holds several unrelated subject areas.
- Two teams contend inside one layer, because the layer and not the feature is the unit of ownership.
