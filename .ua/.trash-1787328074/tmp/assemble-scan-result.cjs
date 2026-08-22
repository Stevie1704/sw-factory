#!/usr/bin/env node

const fs = require('fs');

/** Read and parse a UTF-8 JSON file. */
function readJson(path) {
  return JSON.parse(fs.readFileSync(path, 'utf8'));
}

const scan = readJson('.ua/tmp/ua-scan-files.json');
const imports = readJson('.ua/tmp/ua-import-map-output.json');

if (scan.totalFiles !== scan.files.length) {
  throw new Error(`totalFiles ${scan.totalFiles} does not match files length ${scan.files.length}`);
}
if (!scan.scriptCompleted || !imports.scriptCompleted) {
  throw new Error('A bundled scanner did not report scriptCompleted=true');
}
if (!imports.importMap || typeof imports.importMap !== 'object') {
  throw new Error('Import resolver did not produce an importMap object');
}
for (const file of scan.files) {
  if (!Object.prototype.hasOwnProperty.call(imports.importMap, file.path)) {
    throw new Error(`Import map is missing scanned file ${file.path}`);
  }
}

const result = {
  name: 'sw-factory',
  description: 'No description available',
  languages: Object.keys(scan.stats.byLanguage).sort((a, b) => a.localeCompare(b)),
  frameworks: [],
  files: scan.files,
  totalFiles: scan.totalFiles,
  filteredByIgnore: scan.filteredByIgnore,
  estimatedComplexity: scan.estimatedComplexity,
  importMap: imports.importMap,
};

fs.writeFileSync('.ua/intermediate/scan-result.json', `${JSON.stringify(result, null, 2)}\n`);
console.log(JSON.stringify({
  output: '.ua/intermediate/scan-result.json',
  totalFiles: result.totalFiles,
  filesArrayLength: result.files.length,
  languageCount: result.languages.length,
  importEntries: Object.keys(result.importMap).length,
}));
