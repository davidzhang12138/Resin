import { apiRequest } from "../../lib/api-client";
import type { FreePoolConfig, FreePoolStatusResponse } from "./types";

const basePath = "/api/v1/free-pool";

export async function getFreePoolStatus(): Promise<FreePoolStatusResponse> {
  return apiRequest<FreePoolStatusResponse>(`${basePath}/status`);
}

export async function patchFreePoolConfig(
  patch: Partial<FreePoolConfig>,
): Promise<FreePoolStatusResponse> {
  return apiRequest<FreePoolStatusResponse>(`${basePath}/config`, {
    method: "PATCH",
    body: patch,
  });
}

export async function triggerFreePoolRun(): Promise<{ status: string }> {
  return apiRequest<{ status: string }>(`${basePath}/actions/run`, {
    method: "POST",
  });
}

export async function stopFreePoolRun(): Promise<{ cancelled: boolean }> {
  return apiRequest<{ cancelled: boolean }>(`${basePath}/actions/stop`, {
    method: "POST",
  });
}
