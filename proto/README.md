# The proto contracts

`proto/` holds the contracts mill is built from: `mill.v1`, the store API its
two faces speak; `harness.v1`, the sidecar wire; and `dagger.v1`, vendored
from dagger so mill can call the service it works for. `buf` owns the
generated code in `gen/` and `sidecar/gen`, and `make generate` writes it.
See `docs/architecture.md` for what each contract carries.

## When a breaking change is allowed

Mill has no tagged release. Until it has one, `mill.v1` and `harness.v1` are
mill's own and have no consumer outside this repository, so the contract
moves with the design: a breaking change — a removed or renumbered field, a
retyped one, a deleted message or method — is allowed whenever the design
calls for it. The one obligation is provenance. The node that made the change
is named in the commit that lands it, and the implementer names the contract
change in the result it hands back, so the reviewer judges the change rather
than discovering it.

`dagger.v1` is not mill's to move. It is vendored, and a change to it is a
resync with dagger's copy, not a design decision taken here.

Once a release is tagged, that ends. From the first tag, a breaking change to
`mill.v1` or `harness.v1` needs a new package version — `mill.v2` beside
`mill.v1` — with the old one kept until its consumers are gone, because a
deployed worker and the service it talks to are upgraded separately and a
sidecar built from one release runs against a worker built from another.

## What checks the contract

`buf lint` runs in `make lint`, and so before every commit: the contract has
to be well-formed and every message, field, enum, and value has to state its
meaning.

Compatibility is reported, not enforced. `make lint-contract` runs
`buf breaking` against `main` and names what this tree would break for a
client generated from it. It is out of `make lint` on purpose — a commit gate
that refuses a deliberate contract change is the wrong instrument for a
pre-1.0 contract, and it blocked the harness twice before it was moved. It
runs before a push, and in `make check`, which is what CI or a person runs
before a release. Read its report; a breaking change is a decision, so make
it deliberately.

`make lint` still refuses generated code that lags its proto: `check-generated`
regenerates both templates into a scratch tree and diffs. Generated code is
committed, so a consumer needs no proto toolchain.
