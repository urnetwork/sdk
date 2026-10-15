// For a child process that runs the CLI from its TypeScript sources:
// `node --import ./test/register_ts.ts src/cli.ts …`. The sources import
// each other without extensions, which Node resolves only with this hook.
import { registerTsResolve } from "../../test/ts-resolve.ts";

registerTsResolve();
