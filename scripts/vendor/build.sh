#!/bin/sh
# Rebuilds the Files viewers and editor vendored in web/: CodeMirror 6 (+ languages, our minimap) bundled into one file,
# pdf.js, SheetJS, JSZip and docx-preview copied. The panel loads each the first time a file needs it (files.js).
#   cd scripts/vendor && npm install && ./build.sh
set -e
cd "$(dirname "$0")"
W=../../web
npx esbuild cm-entry.js --bundle --minify --format=iife --target=es2020 --legal-comments=eof \
  --banner:js="/*! CodeMirror 6, Lezer (MIT, Marijn Haverbeke and others) */" --outfile="$W/cm.min.js"
cp node_modules/pdfjs-dist/legacy/build/pdf.min.mjs node_modules/pdfjs-dist/legacy/build/pdf.worker.min.mjs "$W/"
cp node_modules/xlsx/dist/xlsx.full.min.js node_modules/jszip/dist/jszip.min.js "$W/"
npx esbuild node_modules/docx-preview/dist/docx-preview.js --minify --legal-comments=eof --outfile="$W/docx-preview.min.js"
