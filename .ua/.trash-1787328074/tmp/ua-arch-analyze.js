#!/usr/bin/env node

/** Analyze file paths and graph relationships for architectural layer planning. */
const fs = require('fs');
const path = require('path');

function fail(message) {
  console.error(message);
  process.exit(1);
}

function increment(map, key, amount = 1) {
  map[key] = (map[key] || 0) + amount;
}

function topLevelGroup(filePath, commonParts) {
  const parts = filePath.split('/').filter(Boolean);
  if (commonParts.length && parts.slice(0, commonParts.length).join('/') === commonParts.join('/')) {
    return parts[commonParts.length] || 'root';
  }
  return parts.length > 1 ? parts[0] : 'root';
}

function patternFor(group, filePath) {
  const normalized = group.toLowerCase();
  const patterns = {
    routes: 'api', api: 'api', controllers: 'api', endpoints: 'api', handlers: 'api',
    services: 'service', core: 'service', lib: 'service', domain: 'service', logic: 'service',
    models: 'data', db: 'data', data: 'data', persistence: 'data', repository: 'data', entities: 'data',
    components: 'ui', views: 'ui', pages: 'ui', ui: 'ui', layouts: 'ui', screens: 'ui',
    middleware: 'middleware', plugins: 'middleware', interceptors: 'middleware', guards: 'middleware',
    utils: 'utility', helpers: 'utility', common: 'utility', shared: 'utility', tools: 'utility',
    config: 'config', constants: 'config', env: 'config', settings: 'config',
    test: 'test', tests: 'test', spec: 'test', specs: 'test',
    types: 'types', interfaces: 'types', schemas: 'types', contracts: 'types', dtos: 'types',
    hooks: 'hooks', store: 'state', state: 'state', reducers: 'state', actions: 'state', slices: 'state',
    assets: 'assets', static: 'assets', public: 'assets', migrations: 'data',
    management: 'config', commands: 'config', templatetags: 'utility', signals: 'service',
    serializers: 'api', controller: 'api', routers: 'api', composables: 'service', blueprints: 'api',
    mailers: 'service', jobs: 'service', channels: 'service', bin: 'entry', cmd: 'entry',
    internal: 'service', pkg: 'utility', docs: 'documentation', documentation: 'documentation', wiki: 'documentation',
    deploy: 'infrastructure', deployment: 'infrastructure', infra: 'infrastructure', infrastructure: 'infrastructure',
    '.github': 'ci-cd', '.gitlab': 'ci-cd', '.circleci': 'ci-cd', k8s: 'infrastructure', kubernetes: 'infrastructure',
    helm: 'infrastructure', charts: 'infrastructure', terraform: 'infrastructure', tf: 'infrastructure',
    docker: 'infrastructure', sql: 'data', database: 'data', schema: 'data'
  };
  if (patterns[normalized]) return patterns[normalized];
  const base = path.posix.basename(filePath).toLowerCase();
  if (/^main\.go$/.test(base) && filePath.split('/')[0] === 'cmd') return 'entry';
  if (/\.(test|spec)\./.test(base) || /_test\.go$/.test(base)) return 'test';
  if (/\.(md|rst)$/.test(base)) return 'documentation';
  if (/^(go\.mod|go\.sum|cargo\.toml|gemfile|pom\.xml|build\.gradle|package\.json)$/.test(base)) return 'config';
  if (/^(dockerfile|docker-compose\.).*/.test(base) || /\.(tf|tfvars)$/.test(base)) return 'infrastructure';
  if (/\.(sql)$/.test(base)) return 'data';
  return null;
}

function main() {
  const [inputPath, outputPath] = process.argv.slice(2);
  if (!inputPath || !outputPath) fail('Usage: ua-arch-analyze.js <input.json> <output.json>');
  let input;
  try {
    input = JSON.parse(fs.readFileSync(inputPath, 'utf8'));
  } catch (error) {
    fail(`Unable to read input: ${error.message}`);
  }
  const fileNodes = Array.isArray(input.fileNodes) ? input.fileNodes : [];
  const imports = Array.isArray(input.importEdges) ? input.importEdges : [];
  const allEdges = [...(Array.isArray(input.allEdges) ? input.allEdges : []), ...imports];
  if (!fileNodes.length) fail('Input contains no file nodes');

  const fileIds = new Set(fileNodes.map(node => node.id));
  const paths = fileNodes.map(node => node.filePath.split('/').filter(Boolean));
  const commonParts = [];
  for (let index = 0; paths.every(parts => parts[index] && parts[index] === paths[0][index]); index += 1) {
    commonParts.push(paths[0][index]);
  }
  if (commonParts.length === paths.reduce((min, parts) => Math.min(min, parts.length), Infinity)) {
    commonParts.pop();
  }

  const directoryGroups = {};
  const nodeTypeGroups = {};
  const groupById = {};
  for (const node of fileNodes) {
    const group = topLevelGroup(node.filePath, commonParts);
    groupById[node.id] = group;
    (directoryGroups[group] ||= []).push(node.id);
    (nodeTypeGroups[node.type] ||= []).push(node.id);
  }

  const fanIn = {};
  const fanOut = {};
  const adjacency = {};
  const interGroupCounts = {};
  const groupImportsFrom = {};
  const groupImportedBy = {};
  const internalEdges = {};
  const totalEdges = {};
  for (const node of fileNodes) {
    fanIn[node.id] = 0;
    fanOut[node.id] = 0;
    adjacency[node.id] = [];
    internalEdges[groupById[node.id]] ||= 0;
    totalEdges[groupById[node.id]] ||= 0;
  }
  for (const edge of imports) {
    if (!fileIds.has(edge.source) || !fileIds.has(edge.target)) continue;
    const sourceGroup = groupById[edge.source];
    const targetGroup = groupById[edge.target];
    fanOut[edge.source] += 1;
    fanIn[edge.target] += 1;
    adjacency[edge.source].push(edge.target);
    totalEdges[sourceGroup] += 1;
    totalEdges[targetGroup] += 1;
    if (sourceGroup === targetGroup) {
      internalEdges[sourceGroup] += 1;
    } else {
      const key = `${sourceGroup} -> ${targetGroup}`;
      increment(interGroupCounts, key);
      (groupImportsFrom[sourceGroup] ||= new Set()).add(targetGroup);
      (groupImportedBy[targetGroup] ||= new Set()).add(sourceGroup);
    }
  }
  const interGroupImports = Object.entries(interGroupCounts).map(([key, count]) => {
    const [from, to] = key.split(' -> ');
    return {from, to, count};
  });
  const dependencyDirection = [];
  const pairCounts = {};
  for (const item of interGroupImports) {
    const reverse = interGroupCounts[`${item.to} -> ${item.from}`] || 0;
    const key = `${item.from}|${item.to}`;
    if (item.count > reverse && !pairCounts[key]) {
      dependencyDirection.push({dependent: item.from, dependsOn: item.to});
      pairCounts[key] = true;
    }
  }
  const intraGroupDensity = {};
  for (const group of Object.keys(directoryGroups)) {
    intraGroupDensity[group] = {
      internalEdges: internalEdges[group],
      totalEdges: totalEdges[group],
      density: totalEdges[group] ? internalEdges[group] / totalEdges[group] : 0
    };
  }

  const crossCategoryCounts = {};
  for (const edge of allEdges) {
    const source = fileNodes.find(node => node.id === edge.source);
    const target = fileNodes.find(node => node.id === edge.target);
    if (!source || !target || source.type === target.type) continue;
    const key = `${source.type}|${target.type}|${edge.type}`;
    increment(crossCategoryCounts, key);
  }
  const crossCategoryEdges = Object.entries(crossCategoryCounts).map(([key, count]) => {
    const [fromType, toType, edgeType] = key.split('|');
    return {fromType, toType, edgeType, count};
  });

  const patternMatches = {};
  for (const node of fileNodes) {
    const match = patternFor(groupById[node.id], node.filePath);
    if (match && !patternMatches[groupById[node.id]]) patternMatches[groupById[node.id]] = match;
  }
  const infraFiles = fileNodes.filter(node => {
    const base = path.posix.basename(node.filePath).toLowerCase();
    return /^(dockerfile|docker-compose\.).*/.test(base) || /\.(tf|tfvars)$/.test(base) ||
      ['deploy','deployment','infra','infrastructure','docker','k8s','kubernetes','helm','charts','terraform','tf'].includes(groupById[node.id]);
  }).map(node => node.filePath);
  const hasCI = fileNodes.some(node => ['.github','.gitlab','.circleci'].includes(groupById[node.id]) ||
    /(^|\/)(jenkinsfile|\.gitlab-ci\.yml)$/.test(node.filePath));
  const hasDockerfile = fileNodes.some(node => /^dockerfile/i.test(path.posix.basename(node.filePath)));
  const hasCompose = fileNodes.some(node => /^docker-compose\./i.test(path.posix.basename(node.filePath)));
  const hasK8s = fileNodes.some(node => ['k8s','kubernetes','helm','charts'].includes(groupById[node.id]));
  const hasTerraform = fileNodes.some(node => /\.(tf|tfvars)$/.test(node.filePath));

  const dataPipeline = {
    schemaFiles: fileNodes.filter(node => /(^|\/)(schema|schemas|database)\//i.test(node.filePath) || /\.(graphql|gql|proto|prisma|sql)$/.test(node.filePath)).map(node => node.filePath),
    migrationFiles: fileNodes.filter(node => /(^|\/)migrations\//i.test(node.filePath)).map(node => node.filePath),
    dataModelFiles: fileNodes.filter(node => ['data','persistence','repository','models','db'].includes(groupById[node.id])).map(node => node.filePath),
    apiHandlerFiles: fileNodes.filter(node => ['api','routes','controllers','handlers','endpoints'].includes(groupById[node.id])).map(node => node.filePath)
  };
  const documentationGroups = new Set(fileNodes.filter(node => node.type === 'document').map(node => groupById[node.id]));
  const groups = Object.keys(directoryGroups);
  const docCoverage = {
    groupsWithDocs: documentationGroups.size,
    totalGroups: groups.length,
    coverageRatio: groups.length ? documentationGroups.size / groups.length : 0,
    undocumentedGroups: groups.filter(group => !documentationGroups.has(group))
  };
  const setMap = map => Object.fromEntries(Object.entries(map).map(([key, value]) => [key, [...value].sort()]));
  const results = {
    scriptCompleted: true,
    directoryGroups,
    nodeTypeGroups,
    importAdjacency: adjacency,
    interGroupImports,
    intraGroupDensity,
    patternMatches,
    crossCategoryEdges,
    deploymentTopology: {hasDockerfile, hasCompose, hasK8s, hasTerraform, hasCI, infraFiles},
    dataPipeline,
    docCoverage,
    dependencyDirection,
    groupImportsFrom: setMap(groupImportsFrom),
    groupImportedBy: setMap(groupImportedBy),
    fileStats: {
      totalFileNodes: fileNodes.length,
      filesPerGroup: Object.fromEntries(groups.map(group => [group, directoryGroups[group].length])),
      nodeTypeCounts: Object.fromEntries(Object.entries(nodeTypeGroups).map(([type, nodes]) => [type, nodes.length]))
    },
    fileFanIn: fanIn,
    fileFanOut: fanOut
  };
  fs.mkdirSync(path.dirname(outputPath), {recursive: true});
  fs.writeFileSync(outputPath, JSON.stringify(results, null, 2) + '\n');
}

main();
