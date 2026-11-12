import { LitElement, css, html, nothing } from "lit";
import { api, onServerEvents } from "./api.js";
import { changeKind, findingLine, planRows, relativeTime, shortSha, stateInfo } from "./format.js";

/**
 * The whole UI is one element: a status card with the actions, a panel for
 * whatever needs attention in the current state, the dry-run preview, and the
 * deploy key. It refreshes on server events (or by polling).
 */
class GcsApp extends LitElement {
  static properties = {
    status: { state: true },
    conflict: { state: true },
    preview: { state: true },
    deployKey: { state: true },
    busy: { state: true },
    message: { state: true },
    error: { state: true },
    now: { state: true },
  };

  constructor() {
    super();
    this.status = null;
    this.conflict = null;
    this.preview = null;
    this.deployKey = "";
    this.busy = "";
    this.message = "";
    this.error = "";
    this.now = Date.now();
  }

  connectedCallback() {
    super.connectedCallback();
    this.refresh();
    this.loadKey();
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

  async loadKey() {
    try {
      this.deployKey = (await api.deployKey()).public_key;
    } catch {
      this.deployKey = "";
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

  async rotate() {
    if (!confirm("Generate a new deploy key? The app stops using the current one right away; add the new key on GitHub and remove the old one.")) return;
    try {
      this.deployKey = (await api.rotateKey()).public_key;
      this.message = "New deploy key generated. Add it on GitHub (Settings → Deploy keys, with write access).";
    } catch (e) {
      this.message = `Could not rotate the key: ${e.message}`;
    }
  }

  async copyKey() {
    try {
      await navigator.clipboard.writeText(this.deployKey);
      this.message = "Deploy key copied.";
    } catch {
      this.renderRoot.querySelector("#key")?.select();
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
      ${this.preview ? this.renderPreview(this.preview) : nothing}
      ${this.renderKey()}
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
          ${st.state === "bootstrap_review"
            ? html`<button ?disabled=${disabled} @click=${() => this.runSync({ accept_bootstrap: true }, "Syncing")}>Accept first sync</button>`
            : nothing}
          ${this.busy ? html`<span class="muted">${this.busy}…</span>` : nothing}
        </div>
      </section>
    `;
  }

  renderAttention(st) {
    switch (st.state) {
      case "conflict":
        return html`
          <section class="card error">
            <h2>Conflict</h2>
            <p>These files changed on both sides. The box branch was pushed with the box version; merge <code>box</code> into <code>main</code> (locally or on GitHub) and the next sync continues.</p>
            <ul class="mono">${(st.conflict_paths ?? []).map((p) => html`<li>${p}</li>`)}</ul>
            ${this.conflict?.compare_url ? html`<a href=${this.conflict.compare_url} target="_blank" rel="noopener">Open the comparison on GitHub</a>` : nothing}
          </section>
        `;
      case "rejected":
        return html`
          <section class="card error">
            <h2>Change rejected</h2>
            <p>The change from main was rolled back. It is not retried until main changes.</p>
            <pre>${st.last_error || st.rejected?.errors || ""}</pre>
          </section>
        `;
      case "blocked_secrets":
        return html`
          <section class="card error">
            <h2>Possible secrets</h2>
            <p>Nothing was committed or pushed. Move these values to <code>secrets.yaml</code> and use <code>!secret</code>, or add the fingerprint to <code>secret_scan.allow</code> if it is a false positive.</p>
            <ul class="mono">
              ${(st.findings ?? []).map((f) => html`<li>${findingLine(f)} <span class="muted">${f.fingerprint}</span></li>`)}
            </ul>
          </section>
        `;
      case "bootstrap_review":
        return html`
          <section class="card warn">
            <h2>Review the first sync</h2>
            <p>The <code>box</code> branch on the repo shows exactly how this Home Assistant differs from <code>main</code>. Merge it into main, or use <em>Accept first sync</em> to make main match this box.</p>
          </section>
        `;
      case "suspicious_snapshot":
        return html`
          <section class="card warn">
            <h2>Snapshot stopped</h2>
            <pre>${st.last_error}</pre>
            <p>If the deletions are intended, confirm them once:</p>
            <button ?disabled=${Boolean(this.busy)} @click=${() => confirm("Commit the deletions from this box?") && this.runSync({ allow_mass_delete: true })}>Sync allowing the deletions</button>
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

  renderPreview(rep) {
    const toBox = planRows(rep.plan);
    return html`
      <section class="card">
        <h2>Preview <button class="link" @click=${() => (this.preview = null)}>close</button></h2>
        <p class="muted">What a sync would do now. Nothing was written.</p>
        ${rep.merge_kind ? html`<p>Merge: <code>${rep.merge_kind}</code></p>` : nothing}
        <h3>From the box to the repo</h3>
        ${rep.changes?.length
          ? html`<ul class="mono">${rep.changes.map((c) => html`<li>${changeKind(c.kind)} ${c.path}</li>`)}</ul>`
          : html`<p class="muted">Nothing.</p>`}
        <h3>From main to the box</h3>
        ${toBox.length
          ? html`<ul class="mono">${toBox.map((r) => html`<li>${r.kind} ${r.path}</li>`)}</ul>`
          : html`<p class="muted">Nothing.</p>`}
        ${rep.conflict_paths?.length
          ? html`<h3>Would conflict</h3><ul class="mono">${rep.conflict_paths.map((p) => html`<li>${p}</li>`)}</ul>`
          : nothing}
        ${rep.skipped?.length
          ? html`<h3>Skipped</h3><ul class="mono">${rep.skipped.map((s) => html`<li>${s.repo_path} (${s.reason})</li>`)}</ul>`
          : nothing}
        ${rep.findings?.length
          ? html`<h3>Possible secrets</h3><ul class="mono">${rep.findings.map((f) => html`<li>${findingLine(f)}</li>`)}</ul>`
          : nothing}
      </section>
    `;
  }

  renderKey() {
    return html`
      <section class="card">
        <h2>Deploy key</h2>
        ${this.deployKey
          ? html`
              <p>Add this key to your repository (GitHub → Settings → Deploy keys) with <strong>Allow write access</strong>.</p>
              <textarea id="key" readonly rows="2" class="mono">${this.deployKey}</textarea>
              <div class="actions">
                <button @click=${() => this.copyKey()}>Copy</button>
                <button @click=${() => this.rotate()}>Generate a new key</button>
              </div>
            `
          : html`<p class="muted">The app has not created its deploy key yet.</p>`}
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
    textarea {
      width: 100%;
      box-sizing: border-box;
      resize: none;
      background: var(--gcs-bg);
      color: var(--gcs-text);
      border: 1px solid var(--gcs-border);
      border-radius: 8px;
      padding: 8px;
    }
    a {
      color: var(--gcs-primary);
    }
  `;
}

customElements.define("gcs-app", GcsApp);
