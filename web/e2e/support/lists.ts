// A stub block-list publisher for the end-to-end run: a plain HTTP server
// on 127.0.0.1 serving hosts-format lists, so a BlockList with a url
// source can be fetched without leaving the host (the node runs with
// blocking.allowLocalListURLs). /hosts.txt is a fixed list; /list/<id>.txt
// names the test's id in every host, so parallel tests own their names.
import http from 'node:http';

/** The hosts of the fixed list /hosts.txt. */
export const URL_LIST = { path: '/hosts.txt', hosts: ['urlad1.example.net', 'urlad2.example.net'] };

/** The two hosts of /list/<id>.txt. */
export function listHosts(id: string): [string, string] {
  return [`urlad1-${id}.example.net`, `urlad2-${id}.example.net`];
}

function hostsBody(hosts: string[]): string {
  return ['# kudurru e2e stub list', ...hosts.map((h) => `0.0.0.0 ${h}`), ''].join('\n');
}

export interface ListServer {
  /** http://127.0.0.1:<port> */
  url: string;
  close: () => Promise<void>;
}

export async function startListServer(): Promise<ListServer> {
  const srv = http.createServer((req, res) => {
    const path = (req.url ?? '/').split('?')[0] ?? '/';
    const m = /^\/list\/([a-z0-9-]{1,40})\.txt$/.exec(path);
    const body = path === URL_LIST.path ? hostsBody(URL_LIST.hosts) : m?.[1] ? hostsBody(listHosts(m[1])) : null;
    if (body === null) {
      res.writeHead(404, { 'Content-Type': 'text/plain' });
      res.end('not found\n');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8', 'Cache-Control': 'no-store' });
    res.end(req.method === 'HEAD' ? undefined : body);
  });
  await new Promise<void>((resolve, reject) => {
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => resolve());
  });
  const a = srv.address();
  const port = typeof a === 'object' && a ? a.port : 0;
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => srv.close(() => resolve())),
  };
}
