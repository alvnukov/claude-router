export interface FilterRules {
  filters?: Record<string, boolean>;
  domains?: string[];
  networks?: string[];
  [key: string]: unknown;
}
export function editableRules(value: unknown): value is FilterRules {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const r = value as Record<string, unknown>;
  for (const key of ["domains", "networks"]) {
    if (key in r && (!Array.isArray(r[key]) || !(r[key] as unknown[]).every(v => typeof v === "string"))) return false;
  }
  if ("filters" in r && (!r.filters || typeof r.filters !== "object" || Array.isArray(r.filters) || !Object.values(r.filters).every(v => typeof v === "boolean"))) return false;
  return true;
}
export type PrivacyOperation = "mask" | "detect";
export interface FilterProfile { id: string; name: string; enabled: boolean; mode?: PrivacyOperation; rules: FilterRules }
export interface Binding { kind: string; target: string; profile: string }
export interface PrivacyConfig { version: number; enabled: boolean; default: string; profiles: FilterProfile[]; bindings: Binding[] }
export interface ProfileSnapshot { status: string; revision: string; config: PrivacyConfig | null }
export interface PrivacyMetrics {
  traffic?: {status:string;enabled:boolean;protected:number;detected:number;findings:Record<string,number>;bypassed:number;rejected:number;restored:number;active:number;buffered:boolean};
  trafficApplied: boolean; started: string; checks: number; detects: number; restores: number; rejected: number;
  masked: Record<string, number>; detected: Record<string, number>; active: number; inputLimit: number; ttlSeconds: number;
  events: { at: string; operation: string; outcome: string; durationMs: number; inputBytes: number; outputBytes: number; replacements: number; findings: number }[];
}
export interface PreviewResult {
  id: string; output: string; expires: string; roundtrip: boolean; enabled: boolean; operation?: PrivacyOperation;
  masked: Record<string, number>; detected?: Record<string, number>; unmasked: Record<string, number>; unexpected: number;
  inputBytes: number; outputBytes: number; durationMs: number;
}
export const detectNotice = "Режим детекта не защищает данные. Исходные данные уходят модели без маскирования. Используйте только заведомо несекретные тестовые данные.";
export const filters = [
  { id: "ipv4", name: "IPv4 и подсети", group: "Инфраструктура", description: "Частные, link-local и явно заданные публичные сети. Сохраняет формат и длину префикса; адреса восстанавливаются по снимку запроса." },
  { id: "ipv6", name: "IPv6 и подсети", group: "Инфраструктура", description: "ULA, link-local и заданные сети IPv6. Публичные адреса вне списка сетей остаются видимыми." },
  { id: "mac", name: "MAC-адреса", group: "Инфраструктура", description: "Адреса оборудования с двоеточиями, дефисами и точками. Сохраняет структуру записи." },
  { id: "host", name: "Внутренние домены", group: "Инфраструктура", description: "Имена в указанных доменах и их поддоменах. Для отдельных имён серверов без домена используйте словарь." },
  { id: "email", name: "Электронная почта", group: "Идентичности", description: "Адреса в указанных доменах. Почта вне списка автоматически не скрывается; добавьте её в словарь или шаблоны." },
  { id: "phone", name: "Телефоны", group: "Идентичности", description: "Международные номера с + и 8–15 цифрами. Формат сохраняется; локальные номера требуют своих правил." },
  { id: "dictionary", name: "Имена и логины", group: "Идентичности", description: "Явный словарь персон, организаций, логинов, проектов, адресов и серверов. Также имя пользователя и компьютера. Это словарь, а не семантическое NER-распознавание." },
  { id: "secret", name: "Пароли и ключи", group: "Секреты", description: "Известные префиксы ключей, Bearer, URL-пароли и значения в контексте password / api_key. Секрет заменяется маркером; его содержимое модель не получает." },
  { id: "patterns", name: "Свои шаблоны", group: "Контекст", description: "Регулярные выражения RE2 с типом сущности и выбранной группой. Полезно для внутренних форматов идентификаторов." },
  { id: "fields", name: "Типизированные поля", group: "Контекст", description: "JSON-пути назначают тип целому строковому полю. В аргументах инструмента изменённый псевдоним или неверный тип вызывает отказ, без автокоррекции." },
  { id: "sources", name: "Вложения", group: "Секреты", description: "При sources: withhold заменяет содержимое поддержанных изображений и документов заглушкой. Вложения не анализируются и не восстанавливаются." },
] as const;
export const detectDescriptions: Record<typeof filters[number]["id"], string> = {
  ipv4: "Находит частные, link-local и явно заданные публичные адреса и сети IPv4. Значения остаются без изменений.",
  ipv6: "Находит ULA, link-local и заданные сети IPv6. Публичные адреса вне списка сетей остаются вне детекта.",
  mac: "Считает адреса оборудования с двоеточиями, дефисами и точками, сохраняя исходный текст.",
  host: "Находит имена в указанных доменах и поддоменах. Для отдельных имён серверов без домена нужен словарь.",
  email: "Находит почту в указанных доменах. Для остальных адресов нужны записи в словаре или свои шаблоны.",
  phone: "Считает международные номера с + и 8–15 цифрами. Локальные номера требуют своих правил.",
  dictionary: "Находит формы из явного словаря, имя пользователя и компьютера. Семантическое распознавание неизвестных имён не выполняется.",
  secret: "Находит известные префиксы ключей, Bearer, URL-пароли и значения в контексте password / api_key. Секреты остаются открытыми.",
  patterns: "Считает совпадения регулярных выражений RE2 по выбранной группе и типу сущности, без замены значений.",
  fields: "Применяет тип сущности к строковым полям по настроенным JSON-путям. Исходные значения остаются без изменений.",
  sources: "При sources: withhold считает вложения, которые правило скрыло бы. Содержимое не анализируется и не удаляется.",
};
export const kindName = (kind: string) => ({ ipv4:"IPv4",ipv6:"IPv6",cidr4:"IPv4 / сеть",cidr6:"IPv6 / сеть",mac:"MAC",host:"Домены",email:"Почта",phone:"Телефоны",person:"Персоны",login:"Логины",org:"Организации",unit:"Подразделения",project:"Проекты",address:"Адреса",secret:"Секреты",source:"Вложения" }[kind] || kind);
export const demoRules: FilterRules = {
  domains: ["example.internal"],
  entries: [{kind:"person",forms:["Алексей Ветров"]},{kind:"org",forms:["Северный контур"]},{kind:"login",forms:["ops_demo"]},{kind:"host",forms:["db-primary"]}],
  fields: [{path:"content[*].input.host",kind:"host"},{path:"content[*].input.login",kind:"login"},{path:"content[*].input.password",kind:"secret"}],
  patterns: [{name:"asset-id",kind:"project",regex:"ASSET-[0-9]{4}",group:0}],
};
export const demoProfile: FilterProfile = {id:"demo",name:"Демонстрационный",enabled:true,rules:demoRules};
export const examples = [
  { id:"infra", label:"Серверы и доступ", mode:"text", input:"Алексей Ветров из компании «Северный контур» отвечает за db-primary.\nСервер: 10.24.8.12 · сеть: 10.24.8.0/24\nПочта: ops@example.internal · логин: ops_demo\npassword=Demo-Only-Secret-4821\nПроверь связь db-primary с 10.24.8.12. Идентификатор: ASSET-4821." },
  { id:"contacts", label:"Люди и связи", mode:"text", input:"Алексей Ветров работает в компании «Северный контур».\nОн поддерживает db-primary. Свяжись с Алексеем через ops@example.internal или +7 (999) 123-45-67.\nНеизвестное словарю имя: Марина Смирнова — останется видимым." },
  { id:"tools", label:"Аргументы инструмента", mode:"json", input:JSON.stringify({messages:[{role:"assistant",content:[{type:"tool_use",id:"demo-tool",name:"connect",input:{host:"db-primary",login:"ops_demo",password:"Demo-Only-Secret-4821"}}]}]},null,2) },
] as const;
export const bytes = (n: number) => n < 1024 ? `${n} Б` : `${(n / 1024).toFixed(1)} КиБ`;
export async function privacyAPI<T>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const r = await fetch("/api/ui/privacy" + path, { method:body === undefined ? "GET" : "POST", headers:{"Content-Type":"application/json",Accept:"application/json"}, body:body === undefined ? undefined : JSON.stringify(body), signal, cache:"no-store" });
  const data = await r.json();
  if (!r.ok) throw new Error(typeof data.error === "string" ? data.error : "Локальная проверка недоступна");
  return data as T;
}
