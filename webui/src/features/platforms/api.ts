import { apiRequest } from "../../lib/api-client";
import type {
  LeaseResponse,
  PageResponse,
  Platform,
  PlatformCreateInput,
  PlatformUpdateInput,
  ReassignLeaseInput,
} from "./types";

const basePath = "/api/v1/platforms";

type ApiPlatform = Omit<Platform, "regex_filters" | "region_filters"> & {
  regex_filters?: string[] | null;
  region_filters?: string[] | null;
  max_node_reference_latency?: string | null;
  routable_node_count?: number | null;
  reverse_proxy_miss_action?: Platform["reverse_proxy_miss_action"] | null;
  reverse_proxy_empty_account_behavior?: Platform["reverse_proxy_empty_account_behavior"] | null;
  reverse_proxy_fixed_account_header?: string | null;
  passive_circuit_breaker_disabled?: boolean | null;
};

function parseMissAction(raw: ApiPlatform["reverse_proxy_miss_action"]): Platform["reverse_proxy_miss_action"] {
  if (raw === "TREAT_AS_EMPTY" || raw === "REJECT") {
    return raw;
  }
  throw new Error(`invalid reverse_proxy_miss_action: ${String(raw)}`);
}

function normalizePlatform(raw: ApiPlatform): Platform {
  return {
    ...raw,
    reverse_proxy_miss_action: parseMissAction(raw.reverse_proxy_miss_action),
    max_node_reference_latency:
      typeof raw.max_node_reference_latency === "string" ? raw.max_node_reference_latency : "",
    regex_filters: Array.isArray(raw.regex_filters) ? raw.regex_filters : [],
    region_filters: Array.isArray(raw.region_filters) ? raw.region_filters : [],
    routable_node_count: typeof raw.routable_node_count === "number" ? raw.routable_node_count : 0,
    reverse_proxy_empty_account_behavior:
      raw.reverse_proxy_empty_account_behavior === "RANDOM" ||
      raw.reverse_proxy_empty_account_behavior === "FIXED_HEADER" ||
      raw.reverse_proxy_empty_account_behavior === "ACCOUNT_HEADER_RULE"
        ? raw.reverse_proxy_empty_account_behavior
        : "RANDOM",
    reverse_proxy_fixed_account_header:
      typeof raw.reverse_proxy_fixed_account_header === "string" ? raw.reverse_proxy_fixed_account_header : "",
    passive_circuit_breaker_disabled:
      typeof raw.passive_circuit_breaker_disabled === "boolean" ? raw.passive_circuit_breaker_disabled : false,
  };
}

function normalizePlatformPage(raw: PageResponse<ApiPlatform>): PageResponse<Platform> {
  return {
    ...raw,
    items: raw.items.map(normalizePlatform),
  };
}

export type ListPlatformsPageInput = {
  limit?: number;
  offset?: number;
  keyword?: string;
};

export async function listPlatforms(input: ListPlatformsPageInput = {}): Promise<PageResponse<Platform>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 50),
    offset: String(input.offset ?? 0),
    sort_by: "name",
    sort_order: "asc",
  });
  const keyword = input.keyword?.trim();
  if (keyword) {
    query.set("keyword", keyword);
  }

  const data = await apiRequest<PageResponse<ApiPlatform>>(`${basePath}?${query.toString()}`);
  return normalizePlatformPage(data);
}

export async function getPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`);
  return normalizePlatform(data);
}

export async function createPlatform(input: PlatformCreateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(basePath, {
    method: "POST",
    body: input,
  });
  return normalizePlatform(data);
}

export async function updatePlatform(id: string, input: PlatformUpdateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`, {
    method: "PATCH",
    body: input,
  });
  return normalizePlatform(data);
}

export async function deletePlatform(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}`, {
    method: "DELETE",
  });
}

export async function resetPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}/actions/reset-to-default`, {
    method: "POST",
  });
  return normalizePlatform(data);
}

export async function rebuildPlatform(id: string): Promise<void> {
  await apiRequest<{ status: "ok" }>(`${basePath}/${id}/actions/rebuild-routable-view`, {
    method: "POST",
  });
}

export async function clearAllPlatformLeases(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}/leases`, {
    method: "DELETE",
  });
}

export type ListPlatformLeasesInput = {
  account?: string;
  fuzzy?: boolean;
  sort_by?: "account" | "expiry" | "last_accessed";
  sort_order?: "asc" | "desc";
  limit?: number;
  offset?: number;
};

export async function listPlatformLeases(
  platformId: string,
  input: ListPlatformLeasesInput = {},
): Promise<PageResponse<LeaseResponse>> {
  const params = new URLSearchParams();
  if (input.account) params.set("account", input.account);
  if (input.fuzzy) params.set("fuzzy", "true");
  if (input.sort_by) params.set("sort_by", input.sort_by);
  if (input.sort_order) params.set("sort_order", input.sort_order);
  if (input.limit !== undefined) params.set("limit", String(input.limit));
  if (input.offset !== undefined) params.set("offset", String(input.offset));
  const qs = params.toString();
  const path = `${basePath}/${platformId}/leases${qs ? `?${qs}` : ""}`;
  return apiRequest<PageResponse<LeaseResponse>>(path);
}

export async function reassignLease(
  platformId: string,
  account: string,
  input: ReassignLeaseInput,
): Promise<LeaseResponse> {
  return apiRequest<LeaseResponse>(
    `${basePath}/${platformId}/leases/${encodeURIComponent(account)}`,
    { method: "PUT", body: input },
  );
}

export async function deleteLease(platformId: string, account: string): Promise<void> {
  await apiRequest<void>(
    `${basePath}/${platformId}/leases/${encodeURIComponent(account)}`,
    { method: "DELETE" },
  );
}
