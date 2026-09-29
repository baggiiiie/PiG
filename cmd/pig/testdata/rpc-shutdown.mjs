import { appendFileSync } from "node:fs";

// Records every lifecycle event the extension receives. echo_bridge reports a
// tool update and stays in flight until its run is aborted. session_shutdown
// reports that it started through a notification. Then RPC_SHUTDOWN_BLOCK
// makes it wait on a promise that never settles, RPC_SHUTDOWN_HOLD also keeps
// the event loop alive, and RPC_SHUTDOWN_DIALOG makes it wait on a select.
// The ask command waits on a select; the quit command calls ctx.shutdown().
export default function (pi) {
  if (process.env.RPC_SHUTDOWN_BAD_RESOURCE) {
    pi.on("resources_discover", () => ({ skillPaths: ["file:///a%2Fb"] }));
  }
  const record = (name) => appendFileSync(process.env.RPC_SHUTDOWN_REPORT, name + "\n");
  const events = ["agent_start", "turn_start", "message_start", "message_end", "tool_execution_start", "tool_execution_end", "turn_end", "agent_end", "agent_settled"];
  for (const event of events) {
    pi.on(event, () => record(event));
  }
  if (process.env.RPC_SHUTDOWN_TAIL_DELAY) {
    pi.on("turn_end", async () => {
      await new Promise(resolve => setTimeout(resolve, 50));
      record("tail_timer");
    });
  }
  if (process.env.RPC_SHUTDOWN_ON_SETTLED) {
    pi.on("agent_settled", (_event, ctx) => ctx.shutdown());
  }
  pi.on("session_shutdown", async (_event, ctx) => {
    record("session_shutdown");
    ctx.ui.notify("session_shutdown started", "info");
    if (process.env.RPC_SHUTDOWN_BLOCK) {
      await new Promise(() => {});
    }
    if (process.env.RPC_SHUTDOWN_HOLD) {
      await new Promise(() => {
        setInterval(() => {}, 60_000);
      });
    }
    if (process.env.RPC_SHUTDOWN_DIALOG) {
      record("dialog:" + (await ctx.ui.select("Shutdown dialog", ["keep"])));
    }
  });
  pi.registerCommand("ask", {
    description: "Wait on a select",
    handler: async (_args, ctx) => {
      record("ask:" + (await ctx.ui.select("Ask", ["yes"])));
    },
  });
  pi.registerCommand("quit", {
    description: "Request shutdown",
    handler: async (_args, ctx) => {
      record("quit");
      if (_args === "title") ctx.ui.setTitle("quitting");
      ctx.shutdown();
    },
  });
  pi.registerTool({
    name: "echo_bridge",
    label: "Blocking tool",
    description: "Wait until the run is aborted.",
    parameters: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
    async execute(_id, _args, signal, onUpdate) {
      record("tool_running");
      onUpdate?.({ content: [{ type: "text", text: "running" }], details: {} });
      await new Promise((resolve) => {
        if (signal?.aborted) return resolve();
        signal?.addEventListener("abort", () => resolve(), { once: true });
      });
      return { content: [{ type: "text", text: "aborted" }] };
    },
  });
}
