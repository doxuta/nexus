import { ApiError } from "@/lib/api";

/**
 * True when the route itself is not registered on the server yet (engine slice not landed).
 * gin answers a bare 404 text body, and req() stamps that as code "UNKNOWN"; a real handler 404 carries a named
 * code (VERSION_NOT_FOUND, TENANT_NOT_FOUND ...). Callers show an honest "chưa có" line, never a dead button.
 */
// ponytail: heuristic; đổi ở đây một chỗ nếu gin trả mã
export function isEngineMissing(e: unknown): boolean {
  return e instanceof ApiError && e.status === 404 && e.code === "UNKNOWN";
}
