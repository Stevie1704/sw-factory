#!/usr/bin/env node

const fs = require('fs');

function fail(message) {
  console.error(message);
  process.exit(1);
}

function main() {
  const inputPath = process.argv[2];
  const outputPath = process.argv[3];
  if (!inputPath || !outputPath) fail('Usage: ua-tour-analyze.js input.json output.json');

  let data;
  try {
    data = JSON.parse(fs.readFileSync(inputPath, 'utf8'));
  } catch (error) {
    fail(`Could not read input JSON: ${error.message}`);
  }
  if (!Array.isArray(data.nodes) || !Array.isArray(data.edges) || !Array.isArray(data.layers)) {
    fail('Input must contain nodes, edges, and layers arrays');
  }

  const nodes = data.nodes;
  const edges = data.edges;
  const byId = new Map(nodes.map(node => [node.id, node]));
  const fanIn = new Map(nodes.map(node => [node.id, 0]));
  const fanOut = new Map(nodes.map(node => [node.id, 0]));
  const imports = new Map(nodes.map(node => [node.id, []]));
  const adjacency = new Map(nodes.map(node => [node.id, new Set()]));

  for (const edge of edges) {
    if (!byId.has(edge.source) || !byId.has(edge.target)) continue;
    fanIn.set(edge.target, fanIn.get(edge.target) + 1);
    fanOut.set(edge.source, fanOut.get(edge.source) + 1);
    adjacency.get(edge.source).add(edge.target);
    adjacency.get(edge.target).add(edge.source);
    if ((edge.type === 'imports' || edge.type === 'calls') && edge.direction === 'forward') {
      imports.get(edge.source).push(edge.target);
    }
  }

  const rank = (map, key) => [...nodes]
    .map(node => ({id: node.id, [key]: map.get(node.id), name: node.name}))
    .sort((a, b) => b[key] - a[key] || a.id.localeCompare(b.id))
    .slice(0, 20);

  const codeNames = new Set(['index.ts','index.js','main.ts','main.js','app.ts','app.js','server.ts','server.js','mod.rs','main.go','main.py','main.rs','manage.py','app.py','wsgi.py','asgi.py','run.py','__main__.py','Application.java','Main.java','Program.cs','config.ru','index.php','App.swift','Application.kt','main.cpp','main.c']);
  const maxFanOut = Math.max(0, ...fanOut.values());
  const sortedFanOut = [...fanOut.values()].sort((a,b) => a-b);
  const sortedFanIn = [...fanIn.values()].sort((a,b) => a-b);
  const percentile = (values, fraction) => values[Math.min(values.length - 1, Math.floor((values.length - 1) * fraction))] ?? 0;
  const topTenThreshold = percentile(sortedFanOut, 0.9);
  const bottomQuarterThreshold = percentile(sortedFanIn, 0.25);
  const candidates = nodes.map(node => {
    let score = 0;
    const isCode = node.type === 'file';
    if (isCode && codeNames.has(node.name)) score += 3;
    const depth = node.filePath.split('/').length;
    if (isCode && depth <= 2) score += 1;
    if (isCode && fanOut.get(node.id) >= topTenThreshold && maxFanOut > 0) score += 1;
    if (isCode && fanIn.get(node.id) <= bottomQuarterThreshold) score += 1;
    if (node.type === 'document' && node.filePath === 'README.md') score += 5;
    else if (node.type === 'document' && node.filePath.split('/').length === 1 && node.filePath.endsWith('.md')) score += 2;
    return {id: node.id, score, name: node.name, summary: node.summary};
  }).sort((a,b) => b.score - a.score || a.id.localeCompare(b.id)).slice(0, 5);

  const topCode = candidates.find(candidate => byId.get(candidate.id).type === 'file');
  const startNode = topCode ? topCode.id : null;
  const order = [];
  const depthMap = {};
  const byDepth = {};
  if (startNode) {
    const queue = [startNode];
    depthMap[startNode] = 0;
    while (queue.length) {
      const current = queue.shift();
      order.push(current);
      const depth = depthMap[current];
      (byDepth[depth] ||= []).push(current);
      for (const next of imports.get(current)) {
        if (depthMap[next] === undefined) {
          depthMap[next] = depth + 1;
          queue.push(next);
        }
      }
    }
  }

  const nonCodeFiles = {documentation: [], infrastructure: [], data: [], config: []};
  for (const node of nodes) {
    const item = {id: node.id, name: node.name, type: node.type, summary: node.summary};
    if (node.type === 'document') nonCodeFiles.documentation.push(item);
    else if (['service','pipeline','resource'].includes(node.type)) nonCodeFiles.infrastructure.push(item);
    else if (['table','schema','endpoint'].includes(node.type)) nonCodeFiles.data.push(item);
    else if (node.type === 'config') nonCodeFiles.config.push(item);
  }

  const clusters = [];
  const seenPairs = new Set();
  for (const a of nodes) for (const b of nodes) {
    if (a.id >= b.id) continue;
    const pair = `${a.id}\u0000${b.id}`;
    const mutual = adjacency.get(a.id).has(b.id) && adjacency.get(b.id).has(a.id);
    if (mutual && !seenPairs.has(pair)) {
      seenPairs.add(pair);
      clusters.push({nodes: [a.id, b.id], edgeCount: edges.filter(e => (e.source === a.id && e.target === b.id) || (e.source === b.id && e.target === a.id)).length});
    }
  }
  clusters.sort((a,b) => b.edgeCount - a.edgeCount || a.nodes.join().localeCompare(b.nodes.join())).splice(10);

  const result = {
    scriptCompleted: true,
    entryPointCandidates: candidates,
    fanInRanking: rank(fanIn, 'fanIn'),
    fanOutRanking: rank(fanOut, 'fanOut'),
    bfsTraversal: {startNode, order, depthMap, byDepth},
    nonCodeFiles,
    clusters,
    layers: {count: data.layers.length, list: data.layers.map(({id, name, description}) => ({id, name, description}))},
    nodeSummaryIndex: Object.fromEntries(nodes.map(node => [node.id, {name: node.name, type: node.type, summary: node.summary}])),
    totalNodes: nodes.length,
    totalEdges: edges.length
  };
  try {
    fs.writeFileSync(outputPath, JSON.stringify(result, null, 2) + '\n');
  } catch (error) {
    fail(`Could not write output JSON: ${error.message}`);
  }
}

main();
