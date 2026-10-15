// For a child process that runs the CLI: on exit, says on stderr how long
// after its last write to stdout the process exited, as
// "lagdev-test-exit-gap <ms>".
let lastWrite = performance.now();
const write = process.stdout.write.bind(process.stdout) as (...args: unknown[]) => boolean;
process.stdout.write = ((...args: unknown[]) => {
  lastWrite = performance.now();
  return write(...args);
}) as typeof process.stdout.write;
process.on("exit", () => {
  process.stderr.write(`\nlagdev-test-exit-gap ${Math.round(performance.now() - lastWrite)}\n`);
});
