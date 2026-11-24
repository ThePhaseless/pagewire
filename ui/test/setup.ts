// The browser e2e run (PRINTSCAN_E2E=1) drives real Chromium and must not see a fake DOM.
import { afterEach } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";

if (process.env.PRINTSCAN_E2E !== "1") {
  GlobalRegistrator.register({ url: "http://localhost/" });
  // The toast queue is module-level; without this, toasts from one test file show up in the next.
  const { toastQueue } = await import("../src/components/ui");
  afterEach(() => {
    for (const t of [...toastQueue.visibleToasts]) toastQueue.close(t.key);
  });
}
