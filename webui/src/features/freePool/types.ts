export type FreePoolSourceKind = "auto" | "socks5_list" | "subscription";
export type FreePoolRelayMode = "auto" | "relay_only" | "platform" | "direct";

export type FreePoolSource = {
  id: string;
  name: string;
  url: string;
  kind: FreePoolSourceKind;
  enabled: boolean;
};

export type FreePoolConfig = {
  enabled: boolean;
  interval: string;
  sources: FreePoolSource[];
  max_candidates: number;
  max_keep_nodes: number;
  concurrency: number;
  probe_timeout: string;
  max_latency_ms: number;
  https_check_url: string;
  enforce_strict_tls: boolean;
  require_ssh_port22: boolean;
  ssh_check_target: string;
  relay_mode: FreePoolRelayMode;
  relay_platform: string;
  target_subscription_name: string;
  target_subscription_id: string;
  sync_chain_proxy: boolean;
  sync_detour_tag: string;
  sync_ephemeral: boolean;
  sync_incremental_alive: boolean;
  sync_evict_delay: string;
  ensure_platform: boolean;
  target_platform_name: string;
};

export type FreePoolSourceRunStat = {
  id: string;
  name: string;
  kind: string;
  enabled: boolean;
  raw_parsed: number;
  sanitized: number;
  error?: string;
};

export type FreePoolRunStats = {
  fetched_sources: number;
  failed_sources: number;
  raw_parsed: number;
  sanitized: number;
  candidates: number;
  probed: number;
  https_passed: number;
  ssh22_passed: number;
  kept: number;
  used_relay_desc?: string;
  sources?: FreePoolSourceRunStat[];
};

export type FreePoolVerifiedNode = {
  hash: string;
  tag: string;
  protocol: string;
  server: string;
  server_port: number;
  source_id: string;
  source_name: string;
  latency_ms: number;
  ssh22_ok: boolean;
  ssh22_ms?: number;
  verified_at: string;
};

export type FreePoolStatusResponse = {
  config: FreePoolConfig;
  running: boolean;
  phase: string;
  last_run_at?: string;
  last_success_at?: string;
  next_run_at?: string;
  last_error?: string;
  relay_node_count: number;
  target_subscription_id?: string;
  target_platform_id?: string;
  stats: FreePoolRunStats;
  nodes: FreePoolVerifiedNode[];
};
