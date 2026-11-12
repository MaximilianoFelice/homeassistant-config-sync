import { LitElement, css, html, nothing } from "lit";
import { api, onServerEvents } from "./api.js";
import { REVIEWS, changeKind, findingLine, relativeTime, shortSha, stateInfo } from "./format.js";

/**
 * The whole UI is one element: a status card with the actions, a panel for
 * whatever needs attention in the current state, the secret scanner's
 * findings and the dry-run preview. It refreshes on server events (or by
 * polling).
 */
class GcsApp extends LitElement {
  static properties = {
    status: { state: true },
    conflict: { state: true },
    preview: { state: true },
    busy: { state: true },
    message: { state: true },
    error: { state: true },
    now: { state: true },
    connection: { state: true },
  };

  constructor() {
    super();
    this.connection = null;
    this.status = null;
    this.conflict = null;
    this.preview = null;
    this.busy = "";
    this.message = "";
    this.error = "";
    this.now = Date.now();
  }

  connectedCallback() {
    super.connectedCallback();
    this.refresh();
    this.stopEvents = onServerEvents(() => this.refresh());
    this.clock = setInterval(() => (this.now = Date.now()), 30000);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.stopEvents?.();
    clearInterval(this.clock);
  }

  async refresh() {
    try {
      this.status = await api.status();
      this.conflict = this.status.state === "conflict" ? await api.conflict() : null;
      this.error = "";
    } catch (e) {
      this.error = `Could not load the status: ${e.message}`;
    }
  }

  async runSync(opts = {}, label = "Syncing") {
    this.busy = label;
    this.message = "";
    try {
      const { job_id } = await api.sync(opts);
      const job = await this.waitJob(job_id);
      const state = job.report?.state ?? "error";
      this.message = job.error ? `Sync failed: ${job.error}` : `Sync finished: ${stateInfo(state).label}.`;
    } catch (e) {
      this.message = e.status === 409 ? "A sync is already running." : `Sync failed: ${e.message}`;
    } finally {
      this.busy = "";
      await this.refresh();
    }
  }

  async waitJob(id) {
    for (;;) {
      const job = await api.job(id);
      if (job.status !== "running") return job;
      await new Promise((r) => setTimeout(r, 1000));
    }
  }

  async runPreview() {
    this.busy = "Computing the preview";
    this.message = "";
    try {
      this.preview = await api.pending();
    } catch (e) {
      this.message = e.status === 409 ? "A sync is running; try the preview again in a moment." : `Preview failed: ${e.message}`;
    } finally {
      this.busy = "";
    }
  }

  async showConnection() {
    try {
      this.connection = await api.connection();
    } catch (e) {
      this.connection = { error: e.message };
    }
  }

  render() {
    const st = this.status;
    const info = stateInfo(st?.state, st?.syncing || this.busy === "Syncing");
    return html`
      <header>
        <h1>Git Config Sync</h1>
        ${st ? html`<span class="chip ${info.tone}">${info.label}</span>` : nothing}
      </header>
      ${this.error ? html`<p class="banner error">${this.error}</p>` : nothing}
      ${this.message ? html`<p class="banner">${this.message}</p>` : nothing}
      ${st ? this.renderStatus(st, info) : html`<p class="muted">Loading…</p>`}
      ${st ? this.renderAttention(st) : nothing}
      ${st?.findings?.length ? this.renderFindings(st.findings) : nothing}
      ${st?.check_warnings?.length && st.state !== "check_failed" ? this.renderCheckWarnings(st.check_warnings) : nothing}
      ${this.preview ? this.renderPreview(this.preview) : nothing}
      ${st ? this.renderIntegration() : nothing}
    `;
  }

  renderIntegration() {
    const c = this.connection;
    return html`
      <section class="card">
        <h2>Home Assistant integration</h2>
        <p>The optional <code>config_sync</code> integration (from HACS) adds this app's state, a Sync now button and the scanner's findings to Home Assistant. It finds this app by itself: install it, <strong>restart Home Assistant</strong>, then add it from Settings → Devices &amp; services → Discovered.</p>
        <details>
          <summary>It does not show up in Discovered</summary>
          <p>Add it by hand: Settings → Devices &amp; services → Add integration → Git Config Sync, with these values.</p>
          ${c?.error
            ? html`<p class="banner error">${c.error}</p>`
            : c
              ? html`<dl>
                  <dt>Host</dt>
                  <dd class="mono">${c.host}</dd>
                  <dt>Port</dt>
                  <dd class="mono">${c.port}</dd>
                  <dt>API token</dt>
                  <dd class="mono token">${c.token}</dd>
                </dl>`
              : html`<button @click=${() => this.showConnection()}>Show host, port and API token</button>`}
        </details>
      </section>
    `;
  }

  renderStatus(st, info) {
    const disabled = Boolean(this.busy) || st.syncing;
    return html`
      <section class="card">
        <h2>Status</h2>
        <p>${info.help}</p>
        <dl>
          <dt>Last sync</dt>
          <dd>${relativeTime(st.last_sync, this.now)}</dd>
          <dt>box</dt>
          <dd class="mono">${shortSha(st.box_head)}</dd>
          <dt>main</dt>
          <dd class="mono">${shortSha(st.main_head)}</dd>
        </dl>
        ${st.restart_recommended ? html`<p class="banner warn">Some applied changes need a Home Assistant restart.</p>` : nothing}
        <div class="actions">
          <button class="primary" ?disabled=${disabled} @click=${() => this.runSync()}>Sync now</button>
          <button ?disabled=${disabled} @click=${() => this.runPreview()}>Preview</button>
          ${this.busy ? html`<span class="muted">${this.busy}…</span>` : nothing}
        </div>
      </section>
    `;
  }

  renderAttention(st) {
    const list = (items) => html`<ul class="mono">${(items ?? []).map((p) => html`<li>${p}</li>`)}</ul>`;
    switch (st.state) {
      case "conflict":
        return html`
          <section class="card error">
            <h2>Conflict</h2>
            <p>These files changed on both sides. The <code>box</code> branch was pushed with the box's version; merge it into <code>main</code> like any branch and the next sync continues. To resolve this kind of conflict automatically, set <code>merge_options</code> (for example <code>theirs</code>) or a <code>merge=</code> rule in the repo's <code>.gitattributes</code>.</p>
            ${list(st.conflict_paths)}
            ${this.conflict?.compare_url ? html`<a href=${this.conflict.compare_url} target="_blank" rel="noopener">Open the comparison on GitHub</a>` : nothing}
          </section>
        `;
      case "review":
        return html`
          <section class="card warn">
            <h2>Review needed</h2>
            <p>${REVIEWS[st.review] ?? ""}${st.review === "root_changed" && st.prev_root ? html` (was <code>${st.prev_root}</code>)` : nothing}</p>
            <p>Only the <code>box</code> branch was pushed, on top of <code>main</code> if it exists: comparing them shows exactly how this box differs. Leave the real state in <code>main</code> by merging <code>box</code> (to keep <code>main</code> as it is, <code>git merge -s ours box</code>). If the repo had no <code>main</code> yet, create it from <code>box</code>. Syncing resumes after that.</p>
          </section>
        `;
      case "mass_delete":
        return html`
          <section class="card warn">
            <h2>Many deletions held back</h2>
            <p>Applying <code>main</code> would delete ${st.deleted?.length ?? 0} files on this box. Nothing was applied. If it is intended, raise <code>delete_guard_files</code> or <code>delete_guard_percent</code> for one sync.</p>
            ${list(st.deleted?.slice(0, 50))}
          </section>
        `;
      case "too_large":
        return html`
          <section class="card warn">
            <h2>File too large</h2>
            <p>Nothing was committed. Add these files to the repo's <code>.gitignore</code>, or raise <code>max_file_size_mb</code>.</p>
            ${list(st.too_large)}
          </section>
        `;
      case "path_collision":
        return html`
          <section class="card error">
            <h2>Path collides with the app</h2>
            <p><code>main</code> adds top-level paths that already exist in the app's container. Nothing was applied; move or rename them in <code>main</code>.</p>
            ${list(st.collisions)}
          </section>
        `;
      case "blocked_secrets":
        return html`
          <section class="card error">
            <h2>Possible secrets</h2>
            <p>Nothing was committed because <code>secret_scan.mode</code> is <code>block</code>. See the findings below.</p>
          </section>
        `;
      case "check_failed":
        return html`
          <section class="card error">
            <h2>Home Assistant's config has errors</h2>
            <p>The changes from <code>main</code> were applied, but the config check reported errors, so Home Assistant was not reloaded. Fix them in <code>main</code>; the check is repeated every sync until it passes.</p>
            <pre>${st.check_errors}</pre>
          </section>
        `;
      case "error":
        return html`
          <section class="card error">
            <h2>Error</h2>
            <pre>${st.last_error}</pre>
          </section>
        `;
      default:
        return nothing;
    }
  }

  renderFindings(findings) {
    return html`
      <section class="card warn">
        <h2>Possible secrets in the repo</h2>
        <p>The secret scanner found these in what is synced. Move the values out of the repo (for Home Assistant, <code>secrets.yaml</code> and <code>!secret</code>). For a false positive, add its fingerprint to the repo's <code>.gitleaksignore</code>, or end the line with a <code>gitleaks:allow</code> comment.</p>
        <ul class="mono">
          ${findings.map((f) => html`<li>${findingLine(f)}<br /><span class="muted">${f.fingerprint}</span></li>`)}
        </ul>
      </section>
    `;
  }

  renderCheckWarnings(warnings) {
    return html`
      <section class="card">
        <h2>Config check warnings</h2>
        <ul>${warnings.map((w) => html`<li>${w}</li>`)}</ul>
      </section>
    `;
  }

  renderPreview(rep) {
    const rows = (changes) => html`<ul class="mono">${changes.map((c) => html`<li>${changeKind(c.status)} ${c.path}</li>`)}</ul>`;
    return html`
      <section class="card">
        <h2>Preview <button class="link" @click=${() => (this.preview = null)}>close</button></h2>
        <p class="muted">What a sync would do now. Nothing was written.</p>
        <h3>From the box to the repo</h3>
        ${rep.outgoing?.length ? rows(rep.outgoing) : html`<p class="muted">Nothing.</p>`}
        <h3>From main to the box</h3>
        ${rep.incoming?.length ? rows(rep.incoming) : html`<p class="muted">Nothing.</p>`}
        ${rep.conflict_paths?.length
          ? html`<h3>Would conflict</h3><ul class="mono">${rep.conflict_paths.map((p) => html`<li>${p}</li>`)}</ul>`
          : nothing}
        ${rep.too_large?.length ? html`<h3>Too large</h3><ul class="mono">${rep.too_large.map((p) => html`<li>${p}</li>`)}</ul>` : nothing}
        ${rep.state && rep.state !== "idle" ? html`<p>Would end in: <strong>${stateInfo(rep.state).label}</strong></p>` : nothing}
      </section>
    `;
  }

  static styles = css`
    :host {
      display: block;
      max-width: 820px;
      margin: 0 auto;
      padding: 16px;
    }
    header {
      display: flex;
      align-items: center;
      gap: 12px;
      margin-bottom: 8px;
    }
    h1 {
      font-size: 1.4rem;
      font-weight: 500;
      margin: 0;
    }
    h2 {
      font-size: 1.1rem;
      font-weight: 500;
      margin: 0 0 8px;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    h3 {
      font-size: 0.95rem;
      font-weight: 500;
      margin: 12px 0 4px;
    }
    .card {
      background: var(--gcs-card);
      border: 1px solid var(--gcs-border);
      border-radius: 12px;
      padding: 16px;
      margin: 12px 0;
    }
    .card.error {
      border-left: 4px solid var(--gcs-error);
    }
    .card.warn {
      border-left: 4px solid var(--gcs-warn);
    }
    .chip {
      border-radius: 999px;
      padding: 2px 10px;
      font-size: 0.85rem;
      border: 1px solid currentColor;
    }
    .chip.ok {
      color: var(--gcs-ok);
    }
    .chip.warn,
    .chip.info {
      color: var(--gcs-warn);
    }
    .chip.error {
      color: var(--gcs-error);
    }
    .banner {
      border-radius: 8px;
      padding: 8px 12px;
      background: var(--gcs-card);
      border: 1px solid var(--gcs-border);
    }
    .banner.error {
      color: var(--gcs-error);
    }
    .banner.warn {
      color: var(--gcs-warn);
    }
    dl {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 4px 16px;
      margin: 8px 0;
    }
    dt {
      color: var(--gcs-muted);
    }
    dd {
      margin: 0;
    }
    .mono,
    code,
    pre {
      font-family: var(--gcs-mono);
      font-size: 0.85rem;
    }
    pre {
      white-space: pre-wrap;
      word-break: break-word;
      margin: 8px 0;
    }
    ul {
      margin: 4px 0;
      padding-left: 20px;
    }
    .muted {
      color: var(--gcs-muted);
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 8px;
      margin-top: 12px;
    }
    button {
      font: inherit;
      border-radius: 8px;
      border: 1px solid var(--gcs-border);
      background: transparent;
      color: var(--gcs-text);
      padding: 6px 14px;
      cursor: pointer;
    }
    button.primary {
      background: var(--gcs-primary);
      border-color: var(--gcs-primary);
      color: var(--gcs-primary-text);
    }
    button.link {
      border: none;
      padding: 0;
      color: var(--gcs-primary);
      font-size: 0.85rem;
    }
    button:disabled {
      opacity: 0.5;
      cursor: default;
    }
    a {
      color: var(--gcs-primary);
    }
    summary {
      cursor: pointer;
      color: var(--gcs-primary);
    }
    .token {
      word-break: break-all;
    }
  `;
}

customElements.define("gcs-app", GcsApp);
