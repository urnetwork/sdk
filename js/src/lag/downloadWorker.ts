// The download's dedicated Worker (lag.dev/PLAN.md S5): reads a load's
// download streams off the page's main thread, so every chunk is timed as it
// arrives however busy the page is, and hands the chunks over in batches
// (download.ts). downloadSource.ts starts it.

import { LagDownloadWorkerHost, type LagDownloadToPage, type LagDownloadToWorker } from "./download";

// The two members of a dedicated Worker's global scope this uses, typed here
// so the file compiles with the DOM library like the rest of the sdk.
interface DownloadWorkerScope {
  onmessage: ((event: MessageEvent<LagDownloadToWorker>) => void) | null;
  postMessage(message: LagDownloadToPage): void;
}

const scope = globalThis as unknown as DownloadWorkerScope;
const host = new LagDownloadWorkerHost((message) => scope.postMessage(message));
scope.onmessage = (event) => host.receive(event.data);
scope.postMessage({ type: "ready" });
