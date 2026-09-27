// Package privacy masks personal and infrastructure data in Anthropic
// Messages bodies before they leave the machine, and unmasks the answer.
//
// Addresses keep their shape through keyed prefix-preserving bijections
// (Crypto-PAn), so no map is stored for them. Hosts, emails, phones and
// dictionary words map to invented pseudonyms kept per session under
// ROUTER_HOME/privacy/sessions. Secrets become placeholders that live only as
// long as one request. JSON is rewritten token by token over the raw bytes,
// and only the fields on a positive list are touched.
//
// This is a bounded filter, not an execution sandbox or a complete semantic
// recognizer. It is not yet wired into the router. See README.md and
// THREAT_MODEL.md for coverage, explicit disclosures and remaining risks.
// Every detected processing failure is closed: an error in the rules, the key, the map or the
// lock fails the request instead of sending anything unmasked.
package privacy
