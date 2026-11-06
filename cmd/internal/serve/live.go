package serve

// livePage follows a run from outside: headline numbers from /metrics and
// every event from /_factory/events as it happens. One file, no
// dependencies, so it works through a port-forward with nothing else.
const livePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Factory live view</title>
<style>
:root{--bg:#fbfbf9;--fg:#1d1d1b;--muted:#6b6b66;--line:#e4e3de;--card:#fff;--build:#2f6fdf;--review:#8a4fd8;--release:#178a5b;--canary:#c27a00;--finding:#c4372b;--ledger:#5b6470;--model:#0e8a8a}
@media (prefers-color-scheme:dark){:root{--bg:#151514;--fg:#ecebe6;--muted:#9a9993;--line:#2c2c2a;--card:#1d1d1b;--build:#7aa7ff;--review:#c09bff;--release:#4cc792;--canary:#f0b44c;--finding:#ff7d70;--ledger:#a9b1bc;--model:#5fd3d3}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
header{padding:20px 16px 8px;max-width:1100px;margin:auto}h1{font-size:18px;margin:0}header p{margin:4px 0 0;color:var(--muted)}
main{max-width:1100px;margin:auto;padding:8px 16px 40px}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin:12px 0 20px}
.tile{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:10px 12px}.tile b{display:block;font-size:22px;font-variant-numeric:tabular-nums}.tile span{color:var(--muted);font-size:12px}
.feed{background:var(--card);border:1px solid var(--line);border-radius:10px;overflow:hidden}
.feed h2{font-size:13px;margin:0;padding:10px 12px;border-bottom:1px solid var(--line);color:var(--muted);font-weight:600}
.ev{display:grid;grid-template-columns:72px 150px 1fr;gap:10px;padding:7px 12px;border-bottom:1px solid var(--line);font-variant-numeric:tabular-nums}
.ev:last-child{border-bottom:0}.ev time{color:var(--muted)}.ev .k{font-weight:600}.ev .d{overflow-wrap:anywhere;color:var(--fg)}
.k.build{color:var(--build)}.k.spec{color:var(--build)}.k.review{color:var(--review)}.k.routing{color:var(--release)}.k.canary{color:var(--canary)}.k.finding,.k.observe{color:var(--finding)}.k.ledger{color:var(--ledger)}.k.model{color:var(--model)}.k.story{color:var(--fg)}
.story{background:color-mix(in srgb,var(--fg) 6%,transparent)}
@media (max-width:640px){.ev{grid-template-columns:1fr}.ev time{font-size:12px}}
</style></head><body>
<header><h1>Software being built from intent</h1><p id="sub">connecting…</p></header>
<main>
<div class="tiles" id="tiles"></div>
<div class="feed"><h2>What's happening (newest first)</h2><div id="feed"></div></div>
</main>
<script>
const tiles=[
 ["live version",m=>label(m,"reversible_live_version","version")||"none"],
 ["canary",m=>label(m,"reversible_canary_version","version")||"none"],
 ["requests",m=>fmt(sum(m,"reversible_requests_total"))],
 ["server errors",m=>fmt(sum(m,"reversible_requests_total",l=>/code="5/.test(l)))],
 ["builds passed",m=>fmt(sum(m,"reversible_builds_total",l=>/outcome="passed"/.test(l)))],
 ["coding attempts",m=>fmt(sum(m,"reversible_build_attempts_total"))],
 ["unexplained changes",m=>fmt(sum(m,"reversible_review_changes_total",l=>/explained="no"/.test(l)))],
 ["findings",m=>fmt(sum(m,"reversible_findings_total"))],
 ["model calls",m=>fmt(sum(m,"reversible_model_calls_total"))],
 ["model tokens",m=>fmt(sum(m,"reversible_model_tokens_total"))],
 ["ledger records",m=>fmt(sum(m,"reversible_ledger_records_total"))],
];
function parse(t){return t.split("\n").filter(l=>l&&l[0]!="#").map(l=>{const i=l.lastIndexOf(" ");return [l.slice(0,i),+l.slice(i+1)]})}
function sum(m,name,f){return m.filter(([k])=>(k===name||k.startsWith(name+"{"))&&(!f||f(k))).reduce((a,[,v])=>a+v,0)}
function label(m,name,lab){const r=m.find(([k,v])=>k.startsWith(name+"{")&&v===1&&!k.includes('="none"'));if(!r)return"";const x=r[0].match(new RegExp(lab+'="([^"]*)"'));return x?x[1]:""}
function fmt(n){return n>=1e6?(n/1e6).toFixed(1)+"M":n>=1e4?(n/1e3).toFixed(1)+"k":String(Math.round(n))}
async function refresh(){try{const m=parse(await (await fetch("/metrics")).text());
 document.getElementById("tiles").innerHTML=tiles.map(([n,f])=>'<div class="tile"><b>'+esc(f(m))+'</b><span>'+n+'</span></div>').join("")}catch(e){}}
function esc(s){return String(s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]))}
function describe(e){const f=e.fields||{};
 switch(e.kind){
  case"story.step":return f.step;
  case"spec.checked":return "spec "+f.spec+": "+f.blocking+" blocking, "+f.clarify+" clarifying question(s)";
  case"build.started":return "spec "+f.spec+" → "+f.coder+", "+f.examples+" examples";
  case"build.attempt":return "attempt "+f.attempt+": "+f.result.replace("_"," ")+(f.failed?" ("+f.failed.join(", ")+")":"")+(f.error?" "+f.error:"");
  case"build.finished":return "spec "+f.spec+": "+f.outcome+(f.attempts?" after "+f.attempts+" attempt(s)":"")+(f.lines?", "+f.lines+" lines":"");
  case"review.finished":return f.candidate+" vs "+f.live+": "+f.outcome+", "+f.replay_changes+" replay change(s) ("+f.replay_unexplained+" unexplained), "+f.existing_data_changes+" on existing data";
  case"model.call":return f.model+": "+f.outcome+(f.output_tokens!=null?", "+f.input_tokens+" in / "+f.output_tokens+" out, "+(+f.seconds).toFixed(1)+"s":"")+(f.error?" "+f.error:"");
  case"finding":return f.summary;
  default:{const p=Object.entries(f).filter(([k,v])=>typeof v!=="object").map(([k,v])=>k+"="+v);return p.join("  ")}}}
let n=0;const feed=document.getElementById("feed");
function add(e){const d=document.createElement("div");d.className="ev"+(e.kind.startsWith("story")?" story":"");
 d.innerHTML='<time>'+new Date(e.time).toLocaleTimeString()+'</time><span class="k '+e.kind.split(".")[0]+'">'+esc(e.kind)+'</span><span class="d">'+esc(describe(e))+'</span>';
 feed.prepend(d);if(++n>500)feed.lastChild.remove()}
const es=new EventSource("/_factory/events?stream=1");
es.onopen=()=>document.getElementById("sub").textContent="following /_factory/events and /metrics";
es.onerror=()=>document.getElementById("sub").textContent="reconnecting…";
es.onmessage=x=>add(JSON.parse(x.data));
refresh();setInterval(refresh,2000);
</script></body></html>`
