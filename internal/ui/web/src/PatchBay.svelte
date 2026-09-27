<script lang="ts">
  import { onMount, tick } from "svelte";
  import type { UIState, SessionRoute } from "./types";
  import SessionIdentity from "./SessionIdentity.svelte";
  import { sessionTitle } from "./session";
  import LimitMeter from "./LimitMeter.svelte";
  let { data }: { data: UIState } = $props();
  let host: HTMLDivElement;
  let lines = $state<{ d: string; color: number }[]>([]);
  let width = $state(1);
  let height = $state(1);
  const sessions = $derived(data.sessions.slice(0, 4));
  const routes = $derived(sessions.flatMap((session, sessionIndex) =>
    (session.routes?.length ? session.routes : [session]).map(route => ({ route, sessionIndex }))));
  function connection(s: SessionRoute): string {
    return (
      s.connection ||
      (s.model.includes("/")
        ? s.model.split("/")[0]
        : s.route === "cloud"
          ? "anthropic"
          : "")
    );
  }
  function requested(s: SessionRoute): string {
    return s.requestedModel || s.model || "Модель не определена";
  }
  function measure() {
    if (!host) return;
    const box = host.getBoundingClientRect();
    width = box.width;
    height = box.height;
    if (box.width < 700) {
      lines = [];
      return;
    }
    const rects = new Map(
      Array.from(host.querySelectorAll<HTMLElement>("[data-node]")).map(
        (el) => [el.dataset.node, el.getBoundingClientRect()],
      ),
    );
    const next: { d: string; color: number }[] = [];
    for (const [i, { route, sessionIndex }] of routes.entries()) {
      const c = connection(route);
      const index = data.connections.findIndex((x) => x.name === c);
      for (const [from, to] of [
        ["session-" + sessionIndex, "route-" + i],
        ["route-" + i, "connection-" + c],
      ]) {
        const a = rects.get(from);
        const b = rects.get(to);
        if (!a || !b) continue;
        const x1 = a.right - box.left,
          y1 = a.top + a.height / 2 - box.top,
          x2 = b.left - box.left,
          y2 = b.top + b.height / 2 - box.top;
        next.push({
          d: `M${x1} ${y1} C${x1 + (x2 - x1) / 2} ${y1},${x1 + (x2 - x1) / 2} ${y2},${x2} ${y2}`,
          color: Math.max(0, index) % 5,
        });
      }
    }
    lines = next;
  }
  $effect(() => {
    data;
    routes;
    void tick().then(measure);
  });
  onMount(() => {
    const observer = new ResizeObserver(measure);
    observer.observe(host);
    return () => observer.disconnect();
  });
</script>

<section class="patch-bay live-map" aria-label="Карта маршрутизации">
  <div class="section-head">
    <h2>Куда идут запросы</h2>
    <span class="muted">Последние сессии · профиль {data.activeProfile}</span>
  </div>
  <div class="live-map-body" bind:this={host}>
    <svg
      class="patch-wires"
      viewBox={"0 0 " + width + " " + height}
      aria-hidden="true"
      >{#each lines as line}<path
          d={line.d}
          data-color={line.color}
        />{/each}</svg
    >
    <div class="map-column">
      <h3>Сессии Claude Code</h3>
      {#each sessions as session, i}<a
          class="map-node session-node"
          data-node={"session-" + i}
          href={"#/requests?session=" + encodeURIComponent(session.id)}
          ><div class="spread">
            <SessionIdentity {session} />
            <span class="badge"
              >{session.pending
                ? "В полёте: " + session.pending
                : "Завершено"}</span
            >
          </div>
          <p>{session.preview || "Открыть запросы сессии"}</p>
          {#if session.error}<small class="danger">{session.error}</small
            >{/if}</a
        >{/each}{#if !sessions.length}<div class="map-node">
          <h3>Ожидаем первый запрос</h3>
          <p>Отправьте сообщение из Claude Code через роутер.</p>
        </div>{/if}
    </div>
    <div class="map-column routes-column">
      <h3>Маршруты запросов</h3>
      {#each routes as { route, sessionIndex }, i}<a
          class="map-node route-node"
          data-node={"route-" + i}
          href="#/routes"
          ><strong>{requested(route)}</strong>
          <p>
            {route.effort
              ? "Усилие: " + route.effort
              : "Усилие по умолчанию"}
          </p>
          <small>→ {route.model || "Назначение не записано"}</small>
          <p>Сессия · {sessionTitle(sessions[sessionIndex])} · {route.requests} запр.{route.pending ? " · В полёте: " + route.pending : ""}</p></a
        >{/each}{#if !sessions.length}<a class="map-node" href="#/routes"
          ><strong>{data.families.length} семейств моделей</strong>
          <p>Настроить маршруты →</p></a
        >{/if}
    </div>
    <div class="map-column">
      <h3>Подключения и лимиты</h3>
      {#each data.connections as c, i}<article
          class="map-node map-connection"
          data-color={i % 5}
          data-node={"connection-" + c.name}
        >
          <a href="#/connections" class="spread"
            ><strong>{c.displayName || c.name}</strong><span class="socket-dot"
            ></span></a
          >{#if c.error}<p class="danger">
              {c.error}
            </p>{/if}{#each c.limits as limit}<LimitMeter
              {limit}
              now={data.now}
            />{/each}{#if !c.limits.length}<small
              >{c.refreshing
                ? "Обновляем лимиты…"
                : "Лимиты пока неизвестны"}</small
            >{/if}
        </article>{/each}{#if !data.connections.length}<a
          class="map-node"
          href="#/connections"><strong>Добавить подключение →</strong></a
        >{/if}
    </div>
  </div>
  <p class="map-caption">
    Линии показывают все маршруты сессии из сохранённой истории.{data.sessions
      .length > 4
      ? " Показаны 4 последние сессии."
      : ""}
  </p>
</section>
