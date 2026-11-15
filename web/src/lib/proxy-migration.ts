export interface ProxyMigrationReference {
  kind: string;
  id: string;
  name: string;
  revision: number;
  mode?: string;
}

export interface ProxyMigrationPreview {
  subjects: ProxyMigrationReference[];
  configs: ProxyMigrationReference[];
  blockers: string[];
  token: string;
}

export interface ProxyMigrationResult {
  deleted: boolean;
  results: { kind: string; id: string; ok: boolean; error?: string }[];
}

export function migrationResults(payload: unknown): string[] {
  if (!payload || typeof payload !== "object" || !("results" in payload) || !Array.isArray(payload.results)) return [];
  return payload.results.flatMap((row: unknown) => {
    if (!row || typeof row !== "object" || !("kind" in row) || !("id" in row) || !("ok" in row)) return [];
    if (typeof row.kind !== "string" || typeof row.id !== "string" || typeof row.ok !== "boolean") return [];
    const message = "error" in row && typeof row.error === "string" ? row.error : "未完成";
    return [`${row.kind} #${row.id} · ${row.ok ? "已迁移" : message}`];
  });
}
