import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api } from "../api";
import { AppIcon, Spinner } from "../components/common";
import { ImportImage } from "../components/ImportImage";
import type { AppSummary, CategoryCount } from "../types";
import { formatCount } from "../util";

const PAGE_SIZE = 48;

export function CatalogPage() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const category = params.get("category") ?? "";
  const sort = params.get("sort") ?? "popular";

  const [input, setInput] = useState(q);
  const [categories, setCategories] = useState<CategoryCount[]>([]);
  const [items, setItems] = useState<AppSummary[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string>();
  const [refreshing, setRefreshing] = useState(false);
  const [importing, setImporting] = useState(false);

  useEffect(() => {
    api.categories().then(setCategories, () => {});
  }, []);

  // Debounce typing into the URL, which drives the search.
  useEffect(() => {
    const t = setTimeout(() => {
      if (input !== q) update({ q: input || undefined });
    }, 250);
    return () => clearTimeout(t);
  }, [input]);

  useEffect(() => {
    let cancelled = false;
    setItems(null);
    api
      .searchApps({ q, category, sort, limit: PAGE_SIZE })
      .then((res) => {
        if (cancelled) return;
        setItems(res.items);
        setTotal(res.total);
        setError(undefined);
      })
      .catch((e) => !cancelled && setError(e.message));
    return () => {
      cancelled = true;
    };
  }, [q, category, sort]);

  function update(next: Record<string, string | undefined>) {
    const p = new URLSearchParams(params);
    for (const [k, v] of Object.entries(next)) {
      if (v === undefined) p.delete(k);
      else p.set(k, v);
    }
    setParams(p, { replace: true });
  }

  async function loadMore() {
    if (!items) return;
    const res = await api.searchApps({ q, category, sort, offset: items.length, limit: PAGE_SIZE });
    setItems([...items, ...res.items]);
  }

  async function refresh() {
    setRefreshing(true);
    try {
      await api.refreshCatalog();
      setItems(null);
      const res = await api.searchApps({ q, category, sort, limit: PAGE_SIZE });
      setItems(res.items);
      setTotal(res.total);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setRefreshing(false);
    }
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Catalog</h1>
        <div className="btn-row">
          <button className="btn" onClick={() => setImporting((v) => !v)}>
            + Add a Docker image
          </button>
          <Link className="btn" to="/import">
            Import a server
          </Link>
          <Link className="btn" to="/stacks/new">
            Add a Compose stack
          </Link>
          <button className="btn ghost" onClick={refresh} disabled={refreshing}>
            {refreshing ? "Refreshing…" : "Refresh feed"}
          </button>
        </div>
      </div>
      <p className="muted">
        Templates from Unraid Community Applications. Pick one to configure and launch it as a container, or add any
        Docker image. Images you&apos;ve added are under &quot;Custom&quot;.
      </p>
      {importing && <ImportImage onClose={() => setImporting(false)} />}

      <div className="filters">
        <input
          className="search"
          type="search"
          placeholder="Search games, e.g. enshrouded, valheim, minecraft"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          autoFocus
        />
        <select value={category} onChange={(e) => update({ category: e.target.value })}>
          <option value="">All categories</option>
          {categories.map((c) => (
            <option key={c.name} value={c.name}>
              {c.name} ({c.count})
            </option>
          ))}
        </select>
        <select value={sort} onChange={(e) => update({ sort: e.target.value })}>
          <option value="popular">Most downloaded</option>
          <option value="updated">Recently updated</option>
          <option value="name">Name</option>
        </select>
      </div>

      {error && <div className="alert error">{error}</div>}
      {items === null && !error && <Spinner />}
      {items && (
        <p className="muted small">
          {total} {total === 1 ? "app" : "apps"}
        </p>
      )}

      <div className="app-grid">
        {items?.map((a) => (
          <Link key={a.id} to={`/catalog/${a.id}`} className="card app-card">
            <div className="app-card-head">
              <AppIcon src={a.icon} />
              <div>
                <div className="app-name">{a.name}</div>
                <div className="muted small">{a.author || a.repo}</div>
              </div>
            </div>
            <p className="app-overview">{a.overview?.replace(/\*\*/g, "")}</p>
            <div className="muted small app-meta">
              <code>{a.repository}</code>
              {a.downloads ? <span>⬇ {formatCount(a.downloads)}</span> : null}
            </div>
          </Link>
        ))}
      </div>

      {items && items.length < total && (
        <div className="center">
          <button className="btn" onClick={loadMore}>
            Load more
          </button>
        </div>
      )}
    </div>
  );
}
