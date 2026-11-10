// Run independent author feeds concurrently; the Go client retains its shared
// request throttle. Host slots prevent one slow site occupying the whole pool.
export function runAuthorChecks(authors, check, {
  concurrency = 4, perHost = 2, hostFor = author => author.site,
  onProgress = () => {}, onError = () => {},
} = {}) {
  const pending = authors.map(author => ({ author, host: hostFor(author) }));
  const hosts = new Map();
  const total = pending.length;
  let active = 0, completed = 0, failed = 0;
  return new Promise(resolve => {
    const progress = () => onProgress({ completed, total, active, failed });
    function pump() {
      while (active < concurrency) {
        const index = pending.findIndex(job => (hosts.get(job.host) || 0) < perHost);
        if (index < 0) break;
        const { author, host } = pending.splice(index, 1)[0];
        active++; hosts.set(host, (hosts.get(host) || 0) + 1); progress();
        Promise.resolve().then(() => check(author)).catch(error => {
          failed++; onError(author, error);
        }).finally(() => {
          active--; completed++; hosts.set(host, hosts.get(host) - 1);
          progress(); pump();
        });
      }
      if (!pending.length && !active) resolve({ completed, total, failed });
    }
    progress(); pump();
  });
}
