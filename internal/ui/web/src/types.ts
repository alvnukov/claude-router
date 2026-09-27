export interface RouteValue {
  mode: string;
  pool?: string;
  model?: string;
  effort?: string;
}
export interface Profile {
  defaultPool: string;
  name: string;
  familyRoutes: Record<string, Record<string, RouteValue>>;
  routes: Record<string, Record<string, RouteValue>>;
  modelPools: Record<string, { model: string; effort?: string; effortMap?: Record<string, string> }[]>;
}
export interface Limit {
  id: string;
  label: string;
  known: boolean;
  remaining: number;
  reset: string;
  blocked: boolean;
}
export interface Connection {
  name: string;
  displayName: string;
  type: string;
  baseURL: string;
  keySet: boolean;
  connected: boolean;
  pending: boolean;
  refreshing: boolean;
  error: string;
  models: string[];
  limits: Limit[];
  updated: string;
  catalog?: { id: string; name?: string }[];
}
export interface Model {
  key: string;
  provider: string;
  model: string;
  efforts: string[];
}
export interface Route {
  model: string;
  label: string;
  choices: { effort: string; destination: string; inherited: string; configured: boolean }[];
}
export interface Pool {
  name: string;
  type: string;
  maxInputChars: number;
  failover: boolean;
  firstByte: number;
  probeEvery: number;
  members: {
    model: string;
    effort: string;
    effortMap: Record<string, string>;
    efforts: string[];
    cooling: boolean;
  }[];
}
export interface Session {
  requestedModel: string;
  preview: string;
  connection: string;
  effort: string;
  id: string;
  model: string;
  route: string;
  pending: number;
  lastAt: string;
  error: string;
  requests: number;
}
export interface UIState {
  defaultPool: string;
  now: string;
  started: string;
  lifecycle: string;
  activeProfile: string;
  profiles: Profile[];
  connections: Connection[];
  models: Model[];
  families: Route[];
  routes: Route[];
  pools: Pool[];
  efforts: string[];
  summary: { total: number; errors5m: number; pending: number };
  sessions: Session[];
  interception: { enabled: boolean; canRestore: boolean; error: string };
  reloadErrors: string[];
}
export interface RequestItem {
  method: string;
  path: string;
  unrecognized: string;
  fallbackPool: string;
  id: string;
  session: string;
  start: string;
  end: string;
  model: string;
  served: string;
  connection: string;
  route: string;
  status: number;
  pending: boolean;
  failed: boolean;
  durationMs: number;
  preview: string;
  error: string;
}
export interface RequestList {
  items: RequestItem[];
  total: number;
  offset: number;
  limit: number;
}
export interface RequestDetail extends RequestItem {
  captureTruncated: boolean;
  requestNote: string;
  requestedEffort: string | null;
  sentEffort: string | null;
  request: string;
  sent: string;
  response: string;
  attempts: { model: string; error: string; durationMs: number }[];
  headers: Record<string, string>;
  usage: Record<string, number>;
  truncated: boolean;
}
