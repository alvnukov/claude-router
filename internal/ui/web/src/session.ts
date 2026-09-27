import type { Session } from "./types";

export function sessionTitle(session?: Session, id = ""): string {
  return session?.title || (session?.id || id).slice(0, 12) || "Без сессии";
}

export function sessionContext(session?: Session): string {
  return [session?.project, session?.branch].filter(Boolean).join(" · ");
}

export function sessionOption(session: Session): string {
  return [sessionTitle(session), sessionContext(session), session.title ? session.id.slice(0, 8) : ""].filter(Boolean).join(" · ");
}
