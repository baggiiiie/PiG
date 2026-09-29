import { readFile } from "node:fs/promises";
import { createInterface } from "node:readline";
import { importExtension } from "./jiti-loader.mjs";
import { adoptPiProcessIdentity } from "./process-identity.mjs";
import { invalidFactory, loadFailure, reportLoadFailure, Runtime } from "./runtime.mjs";
import { runWithRuntime } from "./state.mjs";

const manifestPath = process.argv[2];
if (!manifestPath) throw new Error("Node cell manifest is required");
const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
if (!Array.isArray(manifest) || manifest.length === 0) throw new Error("Node cell manifest is empty");
adoptPiProcessIdentity();

// pig additive (D20): the host admits factories in configured order while members share one Node process and event bus.
// The host's private stdin channel admits exactly one manifest member at a time. Its next admission follows this member's register handshake and any intervening native factories. Extension traffic stays on each member's own socket.
const admissions = createInterface({ input: process.stdin, crlfDelay: Infinity });
const running = [];
const nativeProviderObjects = new Map();
let next = 0;
for await (const line of admissions) {
  const name = JSON.parse(line);
  const member = manifest[next++];
  if (!member || member.name !== name) throw new Error(`Unexpected Node cell admission: ${name}`);
  process.env.PIG_EXT_NAME = member.name;
  const runtime = new Runtime(member.entry, nativeProviderObjects);
  try {
    await runWithRuntime(runtime, async () => {
      // 0.3.0: replaced by Pi runner wiring
      const install = await importExtension(member.entry);
      if (typeof install !== "function") throw invalidFactory(member.entry);
      await install(runtime.api);
    });
  } catch (error) {
    runtime.discardLoad();
    await reportLoadFailure(process.env[member.sockEnv], loadFailure(error));
    continue;
  }
  runtime.commitLoad();
  process.env.PIG_EXT_SOCKET = process.env[member.sockEnv] || "";
  running.push(runWithRuntime(runtime, () => runtime.run()).catch((error) => {
    console.error(`extension "${runtime.name}" runtime failed: ${error?.stack || error}`);
    process.exitCode = 1;
  }));
}
if (next !== manifest.length) throw new Error("Node cell admission ended before all members loaded");
if (running.length === 0) process.exitCode = 1;
await Promise.all(running);
