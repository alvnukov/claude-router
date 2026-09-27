import type { Session } from "./types";

export interface ModelDestination {
  model: string;
  connection: string;
  requestedModels: string[];
  sessionIndices: number[];
  requests: number;
  pending: number;
}

// The map has one node per actual destination, shared by every visible session.
// The backend retains the per-effort history for request inspection.
export function modelDestinations(sessions: Session[]): ModelDestination[] {
  const destinations = new Map<string, ModelDestination>();
  sessions.forEach((session, sessionIndex) => {
    for (const route of session.routes?.length ? session.routes : [session]) {
      const connection = route.connection ||
        (route.route === "cloud" || route.route === "passthrough" ? "anthropic" : "");
      // Without a connection, model may just be the requested model's fallback.
      const model = connection
        ? route.model.startsWith(connection + "/") ? route.model.slice(connection.length + 1) : route.model
        : "";
      const key = JSON.stringify([connection, model]);
      let destination = destinations.get(key);
      if (!destination) {
        destination = { model, connection, requestedModels: [], sessionIndices: [], requests: 0, pending: 0 };
        destinations.set(key, destination);
      }
      if (!destination.sessionIndices.includes(sessionIndex)) destination.sessionIndices.push(sessionIndex);
      if (route.requestedModel && !destination.requestedModels.includes(route.requestedModel)) destination.requestedModels.push(route.requestedModel);
      destination.requests += route.requests;
      destination.pending += route.pending;
    }
  });
  return [...destinations.values()];
}
