// Captures how real pi's chord builds and parses "reset" subscription updates —
// the oracle behind TestResetUpdatesMatchPi in chord/wire_test.go.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> reset-35180b9df.json 35180b9df
//
// <extraction> is a src extraction of packages/chord at <sha>:
//   git -C ~/.cache/pi-upstream archive -o chord.tar <sha> packages/chord
//   tar -xf chord.tar -C <dir>
// packages/chord/src imports no third-party package, so it needs no
// node_modules. No npm build carries 35180b9df: it landed after v0.87.1.
//
// What is captured:
//   - "real": the reset pi's own RemoteServiceProvider sends when a
//     subscription's delivery buffer overflows (101 pending deliveries), for a
//     singleton, a singleton with a method and two states, a keyed service with
//     two instances, and a singleton withdrawn inside the overflow — as the
//     decoded update (JSON.stringify) and as a fresh encoder's wire update.
//   - "table": parseServiceProviderUpdate's and parseWireServiceProviderUpdate's
//     verdict on each literal — ok, or the message they throw.
import { writeFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/chord/src");
const { BACKGROUND_CONTEXT } = await import(pathToFileURL(path.join(src, "context/index.ts")).href);
const {
	createServiceStateEncoder,
	defineService,
	parseServiceProviderUpdate,
	parseWireServiceProviderUpdate,
	RemoteServiceProvider,
	replicatedState,
} = await import(pathToFileURL(path.join(src, "index.ts")).href);
type ServiceProviderUpdate = { type: string };

type Captured = { name: string; decoded: string; wire: string };
const real: Captured[] = [];

function capture(
	name: string,
	provider: RemoteServiceProvider,
	serviceId: string,
	mode: "singleton" | "keyed",
	drive: () => void,
): void {
	const updates: ServiceProviderUpdate[] = [];
	const subscription = provider.subscribe(serviceId, mode, (update) => updates.push(update));
	const encoder = createServiceStateEncoder();
	encoder.encodeSnapshot(subscription.snapshot);
	drive();
	subscription.activate();
	const reset = updates.find((u) => u.type === "reset");
	if (reset === undefined) throw new Error(`${name}: no reset delivered; got ${updates.map((u) => u.type)}`);
	real.push({ name, decoded: JSON.stringify(reset), wire: JSON.stringify(encoder.encodeUpdate(reset)) });
	subscription.close();
}

// 1. Singleton, one state, overflow after 101 publications.
{
	interface Counter {
		readonly state: unknown;
	}
	const Counter = defineService<Counter>("test.delivery-counter");
	const provider = new RemoteServiceProvider([Counter]);
	const state = replicatedState({ value: 0 });
	provider.provide(Counter, { state } as never);
	capture("singleton-overflow", provider, Counter.id, "singleton", () => {
		for (let value = 1; value <= 101; value += 1) state.replace(BACKGROUND_CONTEXT, { value });
	});
	provider.dispose();
}

// 2. Singleton with a method and two states; one payload has keys in non-sorted insertion order.
{
	interface Mixed {
		readonly ping: unknown;
		readonly state: unknown;
		readonly other: unknown;
	}
	const Mixed = defineService<Mixed>("test.mixed");
	const provider = new RemoteServiceProvider([Mixed]);
	const state = replicatedState({ zeta: 0, alpha: 0 } as Record<string, number>);
	const other = replicatedState({ value: "<&>" });
	provider.provide(Mixed, { ping: () => "pong", state, other } as never);
	capture("singleton-method-and-states", provider, Mixed.id, "singleton", () => {
		for (let value = 1; value <= 101; value += 1) state.replace(BACKGROUND_CONTEXT, { zeta: value, alpha: -value });
	});
	provider.dispose();
}

// 3. Keyed, two instances spawned out of key order.
{
	interface Counter {
		readonly state: unknown;
	}
	const Counter = defineService<Counter>("test.keyed-counter");
	const provider = new RemoteServiceProvider([{ service: Counter, mode: "keyed" }]);
	const b = replicatedState({ value: 0 });
	const a = replicatedState({ value: 0 });
	provider.spawn(Counter, "b", { state: b } as never);
	provider.spawn(Counter, "a", { state: a } as never);
	capture("keyed-overflow", provider, Counter.id, "keyed", () => {
		for (let value = 1; value <= 101; value += 1) b.replace(BACKGROUND_CONTEXT, { value });
	});
	provider.dispose();
}

// 4. Singleton withdrawn inside the overflow: a reset with no instances.
{
	interface Counter {
		readonly state: unknown;
	}
	const Counter = defineService<Counter>("test.withdrawn");
	const provider = new RemoteServiceProvider([Counter]);
	provider.provide(Counter, { state: replicatedState({ value: 0 }) } as never);
	capture("singleton-withdrawn", provider, Counter.id, "singleton", () => {
		for (let value = 1; value <= 100; value += 1)
			provider.replace(Counter, { state: replicatedState({ value }) } as never);
		provider.withdraw(Counter);
	});
	provider.dispose();
}

// Parser verdict table.
const snap = (instances: string) =>
	`{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"singleton","instances":${instances}}}`;
const one = (members: string) => snap(`[{"members":${members}}]`);
const st = (ops: string, seq = "103") => `{"name":"state","kind":"state","sequence":${seq},"ops":${ops}}`;
const cases: Record<string, string> = {
	valid: one(`[${st(`[["r",{"after":103}]]`)}]`),
	zeroOps: one(`[${st(`[]`)}]`),
	twoReplaces: one(`[${st(`[["r",1],["r",2]]`)}]`),
	replaceThenSet: one(`[${st(`[["r",1],["s",["a"],2]]`)}]`),
	setInlinePath: one(`[${st(`[["s",["before"],103]]`)}]`),
	wireShortSet: one(`[${st(`[["s",1]]`)}]`),
	wireIdSet: one(`[${st(`[["s",0,1]]`)}]`),
	define: one(`[${st(`[["#",0,["a"]]]`)}]`),
	replaceArity1: one(`[${st(`[["r"]]`)}]`),
	replaceArity3: one(`[${st(`[["r",1,2]]`)}]`),
	replaceNull: one(`[${st(`[["r",null]]`)}]`),
	rootSplice: one(`[${st(`[["p",[],0,0,[]]]`)}]`),
	rootPermute: one(`[${st(`[["m",[],[]]]`)}]`),
	setEmptyPath: one(`[${st(`[["s",[],1]]`)}]`),
	methodOnly: one(`[{"name":"m","kind":"method"}]`),
	methodAndValid: one(`[{"name":"m","kind":"method"},${st(`[["r",1]]`)}]`),
	methodAndInvalid: one(`[{"name":"m","kind":"method"},${st(`[["s",["a"],1]]`)}]`),
	noInstances: snap(`[]`),
	emptyMembers: one(`[]`),
	keyedValid: `{"type":"reset","snapshot":{"serviceId":"k","mode":"keyed","instances":[{"instance":{"key":"a","generation":1},"members":[${st(`[["r",1]]`)}]}]}}`,
	keyedNoAddress: `{"type":"reset","snapshot":{"serviceId":"k","mode":"keyed","instances":[{"members":[${st(`[["r",1]]`)}]}]}}`,
	singletonTwoInstances: snap(`[{"members":[${st(`[["r",1]]`)}]},{"members":[${st(`[["r",2]]`)}]}]`),
	duplicateMemberNames: one(`[${st(`[["r",1]]`)},${st(`[["r",2]]`)}]`),
	secondInstanceInvalid: snap(`[{"members":[${st(`[["r",1]]`)}]},{"members":[${st(`[["r",1],["r",2]]`)}]}]`),
	secondStateInvalid: one(`[${st(`[["r",1]]`)},${st(`[]`)}]`),
	extraKey: `{"type":"reset","extra":true,"snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[]}}`,
	missingSnapshot: `{"type":"reset"}`,
	nullSnapshot: `{"type":"reset","snapshot":null}`,
	emptySnapshot: `{"type":"reset","snapshot":{}}`,
	arraySnapshot: `{"type":"reset","snapshot":[]}`,
	extraKeyInSnapshot: `{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[],"x":1}}`,
	extraKeyInMember: one(`[{"name":"state","kind":"state","sequence":1,"ops":[["r",1]],"x":1}]`),
	opsNotArray: one(`[${st(`{}`)}]`),
	negativeSequence: one(`[${st(`[["r",1]]`, "-1")}]`),
	fractionalSequence: one(`[${st(`[["r",1]]`, "1.5")}]`),
	zeroSequence: one(`[${st(`[["r",1]]`, "0")}]`),
	emptyName: one(`[{"name":"","kind":"state","sequence":1,"ops":[["r",1]]}]`),
	unknownKind: one(`[{"name":"x","kind":"event"}]`),
	instancesKeyOnUpdate: `{"type":"reset","instances":[],"snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[]}}`,
	unknownMode: `{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"other","instances":[]}}`,
	emptyServiceId: `{"type":"reset","snapshot":{"serviceId":"","mode":"singleton","instances":[]}}`,
	unsortedPayload: one(`[${st(`[["r",{"zeta":1,"alpha":2}]]`)}]`),
	htmlPayload: one(`[${st(`[["r","<&>"]]`)}]`),
	emptyServiceIdAndNonRoot: `{"type":"reset","snapshot":{"serviceId":"","mode":"singleton","instances":[{"members":[${st(`[["s",["a"],1]]`)}]}]}}`,
	extraKeyAndNonRoot: `{"type":"reset","extra":1,"snapshot":{"serviceId":"s","mode":"singleton","instances":[{"members":[${st(`[["s",["a"],1]]`)}]}]}}`,
	extraKeyAndBadSnapshot: `{"type":"reset","extra":1,"snapshot":{"serviceId":"","mode":"singleton","instances":[]}}`,
};

type Verdict = { ok: boolean; error?: string };
const verdict = (parse: (v: unknown) => unknown, literal: string): Verdict => {
	try {
		parse(JSON.parse(literal));
		return { ok: true };
	} catch (error) {
		return { ok: false, error: String((error as Error).message) };
	}
};
const table = Object.entries(cases).map(([name, literal]) => ({
	name,
	literal,
	decoded: verdict(parseServiceProviderUpdate, literal),
	wire: verdict(parseWireServiceProviderUpdate, literal),
}));

writeFileSync(outFile, `${JSON.stringify({ sha, real, table }, null, 1)}\n`);
console.log(`wrote ${real.length} real resets and ${table.length} verdict rows`);
