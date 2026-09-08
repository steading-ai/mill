// The sidecar's entrypoint, compiled to `dist/main.js`.
//
// The worker spawns `node dist/main.js` and speaks harness.v1 to it over
// stdio. This file exists so that path is stable: the compiler mirrors the
// package's layout under `dist/`, and the entrypoint is not something a
// caller should have to follow through it.

import { bootstrap } from "./src/main.js";

await bootstrap();
