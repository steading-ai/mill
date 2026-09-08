// The three-file edit task both arms run: bump a version that appears in three
// files, then prove the JSON one still parses. It needs reads, writes and one
// command, which is exactly the set of verbs under test.

import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const PROMPT = [
  "This project's version is 1.0.0 and it appears in three files: config.json, src/version.py and CHANGELOG.md.",
  "Bump it to 1.1.0 in all three:",
  "  - config.json: the \"version\" field",
  "  - src/version.py: the VERSION constant",
  "  - CHANGELOG.md: add a new '## 1.1.0' section above the 1.0.0 one, with the bullet '- version bumped'",
  "Change nothing else. Then run `python3 -m json.tool config.json` to prove config.json still parses, and say DONE.",
].join("\n");

export function makeWorkspace(): string {
  const root = mkdtempSync(join(tmpdir(), "mcp-task-"));
  mkdirSync(join(root, "src"));
  writeFileSync(join(root, "config.json"), JSON.stringify({ name: "widget", version: "1.0.0", deps: [] }, null, 2) + "\n");
  writeFileSync(join(root, "src/version.py"), 'VERSION = "1.0.0"\n\n\ndef banner() -> str:\n    return f"widget {VERSION}"\n');
  writeFileSync(join(root, "CHANGELOG.md"), "# Changelog\n\n## 1.0.0\n\n- first release\n");
  return root;
}

// What "the task was done" means, checked against the filesystem rather than
// against what the session said.
export function verify(root: string): { ok: boolean; checks: Record<string, boolean> } {
  const read = (p: string) => {
    try {
      return readFileSync(join(root, p), "utf8");
    } catch {
      return "";
    }
  };
  const config = read("config.json");
  let parsed: any = null;
  try {
    parsed = JSON.parse(config);
  } catch {
    /* left null: the check below fails */
  }
  const checks = {
    "config.json parses": parsed !== null,
    "config.json version is 1.1.0": parsed?.version === "1.1.0",
    "config.json otherwise unchanged": parsed?.name === "widget" && Array.isArray(parsed?.deps),
    "version.py bumped": /VERSION\s*=\s*"1\.1\.0"/.test(read("src/version.py")),
    "version.py otherwise unchanged": /def banner\(\) -> str:/.test(read("src/version.py")),
    "changelog has a 1.1.0 section": /##\s*1\.1\.0/.test(read("CHANGELOG.md")),
    "changelog keeps 1.0.0": /##\s*1\.0\.0/.test(read("CHANGELOG.md")),
    "changelog orders 1.1.0 first": read("CHANGELOG.md").indexOf("1.1.0") < read("CHANGELOG.md").indexOf("1.0.0"),
  };
  return { ok: Object.values(checks).every(Boolean), checks };
}
