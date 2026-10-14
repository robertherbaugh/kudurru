// The kudurru license portal on Cloudflare Containers (README.md in this
// directory). The Worker answers https://licenses.kudurru.io and hands
// every request to one Durable Object, LicensePortal, which runs the
// portal's image (license-server/Dockerfile) as its container and proxies
// to the server on port 8790. The server does all the work: sessions,
// CSRF, CORS, the API and the UI. The Worker only
// - redirects plain http to https,
// - writes the client address (src/http.ts forContainer),
// - answers a 503 in the server's shape when the container cannot answer,
// - and, from the cron trigger, keeps the container running and restarts
//   it when a var or secret changed (LicensePortal.tick).

import { Container, getContainer, type StopParams } from '@cloudflare/containers';
import { envDigest, serverEnv } from './env';
import { forContainer, fromPlatform, httpsRedirect, unavailable, type Settings } from './http';
import { describe, log } from './log';

/** The server's port inside the container (PORT in the image). */
const PORT = 8790;

/**
 * The name of the one Durable Object, so of the one container, every
 * request goes to. One instance keeps the server's in-memory state whole:
 * the per-IP sign-in limit (20 a minute), the OIDC discovery cache and
 * the cleanup timer. Sessions, the public form's limits and everything
 * else live in Postgres, so a restart loses nothing else.
 */
const INSTANCE = 'portal';

/** Durable Object storage key: the envDigest the running container started with. */
const STARTED_ENV_KEY = 'started-env-sha256';

/** How long a start may take: getting an instance, then the server listening (after its migrations). */
const START_TIMEOUTS = { instanceGetTimeoutMS: 15_000, portReadyTimeoutMS: 45_000 };

// Public settings are injected by deployment, so Wrangler cannot infer
// them from the committed infrastructure-only config.
type PortalEnv = Env & { PUBLIC_URL: string };

function settings(env: PortalEnv): Settings {
  const formOrigins = (env as unknown as Record<string, unknown>).PUBLIC_FORM_ORIGINS;
  return { publicUrl: env.PUBLIC_URL, formOrigins: typeof formOrigins === 'string' ? formOrigins : undefined };
}

export class LicensePortal extends Container<PortalEnv> {
  override defaultPort = PORT;
  /**
   * Idle time before the container gets SIGTERM. The cron trigger in
   * wrangler.jsonc pings every 5 minutes, so with it the container never
   * idles; 15 minutes is three ticks, so one late or missed tick does not
   * stop it. Without the cron the portal sleeps after 15 quiet minutes,
   * still long enough for someone who used the kudurru.io form to click
   * the link in the mail before the container stops.
   */
  override sleepAfter = '15m';
  /** Postgres (5432), SMTP (465) and the IdP (443): other ports than 80 and 443 need full Internet access. */
  override enableInternet = true;
  /** Readiness only needs the port to answer; /readyz is the cheapest answer the server has. */
  override pingEndpoint = 'localhost/readyz';
  /** The server's configuration, copied from the Worker's vars and secrets (src/env.ts). */
  override envVars = serverEnv(this.env);

  /** Starts the container when needed, then proxies; a failure becomes the Worker's 503, never the cause. */
  override async fetch(request: Request): Promise<Response> {
    if (!(await this.serving())) {
      try {
        await this.startAndWaitForPorts({ ports: PORT, cancellationOptions: START_TIMEOUTS });
      } catch (err) {
        log('error', 'the container did not start', { error: describe(err) });
        return unavailable(request, settings(this.env));
      }
    }
    const res = await this.containerFetch(request, PORT);
    if (!fromPlatform(res)) return res;
    const detail = (await res.text()).slice(0, 300);
    log('error', 'the container did not answer', { status: res.status, detail });
    return unavailable(request, settings(this.env));
  }

  /**
   * The cron trigger's call: restarts the container (SIGTERM, then a fresh
   * start) when the vars or secrets differ from the ones it started with,
   * because a running process keeps its startup environment; then starts
   * it if needed and asks /readyz, which also renews sleepAfter.
   */
  async tick(): Promise<void> {
    const wanted = await envDigest(this.envVars);
    if (this.ctx.container?.running === true) {
      const started = await this.ctx.storage.get<string>(STARTED_ENV_KEY);
      if (started !== wanted) {
        log('info', 'a var or secret changed; restarting the container');
        await this.stop();
        await this.waitForExit(30_000);
      }
    }
    try {
      if (!(await this.serving())) await this.startAndWaitForPorts({ ports: PORT, cancellationOptions: START_TIMEOUTS });
      const res = await this.containerFetch(new Request('http://localhost/readyz'), PORT);
      if (res.status !== 200) log('warn', 'the portal is not ready', { status: res.status, platform: fromPlatform(res) });
      await res.body?.cancel();
    } catch (err) {
      log('error', 'the scheduled check failed', { error: describe(err) });
    }
  }

  override async onStart(): Promise<void> {
    await this.ctx.storage.put(STARTED_ENV_KEY, await envDigest(this.envVars));
    log('info', 'the container started');
  }

  override onStop({ exitCode, reason }: StopParams): void {
    log(exitCode === 0 ? 'info' : 'error', 'the container stopped', { exitCode, reason });
  }

  override onError(error: unknown): never {
    log('error', 'container error', { error: describe(error) });
    throw error;
  }

  /** Running, and ready the last time the Container class checked. */
  private async serving(): Promise<boolean> {
    return this.ctx.container?.running === true && (await this.getState()).status === 'healthy';
  }

  private async waitForExit(ms: number): Promise<void> {
    const until = Date.now() + ms;
    while (this.ctx.container?.running === true && Date.now() < until) await new Promise((r) => setTimeout(r, 250));
  }
}

export default {
  async fetch(request, env): Promise<Response> {
    const redirect = httpsRedirect(request, env.PUBLIC_URL);
    if (redirect) return redirect;
    const forwarded = await forContainer(request);
    try {
      return await getContainer(env.PORTAL, INSTANCE).fetch(forwarded);
    } catch (err) {
      // The Durable Object itself failed (reset by a deploy, overloaded):
      // the request may not have reached the container at all.
      log('error', 'the durable object did not answer', { error: describe(err) });
      return unavailable(request, settings(env));
    }
  },

  async scheduled(_controller, env): Promise<void> {
    try {
      await getContainer(env.PORTAL, INSTANCE).tick();
    } catch (err) {
      log('error', 'the scheduled check failed', { error: describe(err) });
    }
  },
} satisfies ExportedHandler<PortalEnv>;
