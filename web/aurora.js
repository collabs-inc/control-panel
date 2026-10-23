'use strict';

// ---------- aurora ----------
// The aurora borealis behind the chats' composers (the Assistant's, and a terminal chat's in the drawer): a WebGL
// fragment shader, drawn on one canvas at a time. Three curtains at different depths, each with a wavy lower lip
// and vertical rays drifting sideways, over soft teal, blue and violet glows. One number drives it: energy, 0
// (calm) to 1 (the agent working), eased every frame, which speeds the drift, raises the curtains and brightens
// them, so nothing ever jumps. It renders at half resolution (an aurora is soft anyway), runs only while its chat
// is on screen, and holds one still frame for people who ask for less motion.

const AURORA_FS = `
precision mediump float;
uniform vec2 R;
uniform float T;
uniform float E;
float h(vec2 p) { return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }
float n(vec2 p) {
  vec2 i = floor(p), f = fract(p);
  f = f * f * (3.0 - 2.0 * f);
  return mix(mix(h(i), h(i + vec2(1.0, 0.0)), f.x), mix(h(i + vec2(0.0, 1.0)), h(i + vec2(1.0, 1.0)), f.x), f.y);
}
float fbm(vec2 p) {
  float v = 0.0, a = 0.5;
  for (int i = 0; i < 4; i++) { v += a * n(p); p = p * 2.03 + vec2(1.7, 9.2); a *= 0.5; }
  return v;
}
vec3 hue(float k) { // the curtains' own colours: teal, blue, violet
  return k < 0.5 ? vec3(0.18, 1.0, 0.72) : k < 1.5 ? vec3(0.3, 0.62, 1.0) : vec3(0.72, 0.42, 1.0);
}
void main() {
  vec2 uv = gl_FragCoord.xy / R;
  float ar = R.x / R.y;
  float x = uv.x * ar;
  vec3 col = vec3(0.0);
  float al = 0.0;
  // ambient: three wide soft glows drifting along the bottom, for colour under the curtains
  for (int k = 0; k < 3; k++) {
    float fk = float(k);
    float cx = ar * (0.18 + 0.32 * fk) + 0.28 * ar * sin(T * (0.045 + 0.02 * fk) + fk * 2.1);
    float w = ar * (0.32 + 0.08 * fk);
    float g = exp(-pow((x - cx) / w, 2.0) - pow(uv.y / (0.46 + 0.08 * E), 2.0) * 1.6) * (0.3 + 0.16 * E);
    col += hue(fk) * g;
    al += g;
  }
  // curtains: a wavy bright lip, soft vertical rays drifting sideways, fading up into magenta
  for (int k = 0; k < 3; k++) {
    float fk = float(k);
    float s = T * (0.055 + 0.03 * fk);
    float edge = 0.1 + 0.1 * fk + 0.16 * fbm(vec2(x * 0.42 + fk * 4.3 + s, fk * 1.7 + s * 0.55));
    float d = uv.y - edge;
    float ray = smoothstep(0.3, 0.88, fbm(vec2(x * 4.6 + fk * 13.0 - s * 1.7, uv.y * 0.6 - s * 0.5)));
    float reach = 0.32 + 0.14 * fk + 0.1 * E;
    float body = smoothstep(-0.03, 0.02, d) * exp(-max(d, 0.0) / reach * 2.4);
    float lip = exp(-abs(d) * 24.0) * 0.55;
    float a = (body * (0.28 + 0.8 * ray) + lip * (0.6 + 0.4 * ray)) * (0.55 - fk * 0.1) * (0.5 + 0.3 * E);
    vec3 c = mix(hue(fk), vec3(0.95, 0.45, 0.95), smoothstep(0.25, 1.0, d / reach) * 0.6);
    col += c * a;
    al += a;
  }
  float fade = smoothstep(0.82, 0.32, uv.y); // gone well before the chat above the composer
  al = clamp(al * fade, 0.0, 1.0);
  gl_FragColor = vec4(min(col * fade, vec3(al)), al);
}`;

const AU = { gl: null, canvas: null, raf: 0, t: Math.random() * 100, e: 0, goal: 0, last: 0, u: null, ro: null };
const AU_GL = new Map(); // canvas -> its prepared context: { gl, u }

// auroraGL(canvas): the canvas's WebGL context with the shader ready (made once per canvas), or null
function auroraGL(canvas) {
  if (AU_GL.has(canvas)) return AU_GL.get(canvas);
  for (const [c, x] of AU_GL) if (!c.isConnected) { x.gl.getExtension('WEBGL_lose_context')?.loseContext(); AU_GL.delete(c); } // gone from the page: free the GPU
  const gl = canvas.getContext('webgl', { premultipliedAlpha: true, antialias: false, alpha: true });
  if (!gl) return null; // no WebGL: no aurora, the page works the same
  const sh = (type, src) => { const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s); return s; };
  const p = gl.createProgram();
  gl.attachShader(p, sh(gl.VERTEX_SHADER, 'attribute vec2 p; void main() { gl_Position = vec4(p, 0.0, 1.0); }'));
  gl.attachShader(p, sh(gl.FRAGMENT_SHADER, AURORA_FS));
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) return null;
  gl.useProgram(p);
  gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW); // one triangle covers the screen
  const at = gl.getAttribLocation(p, 'p');
  gl.enableVertexAttribArray(at);
  gl.vertexAttribPointer(at, 2, gl.FLOAT, false, 0, 0);
  gl.enable(gl.BLEND);
  gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
  const x = { gl, u: { R: gl.getUniformLocation(p, 'R'), T: gl.getUniformLocation(p, 'T'), E: gl.getUniformLocation(p, 'E') } };
  AU_GL.set(canvas, x);
  return x;
}

// aurora(canvas): draw on this canvas (the Assistant's, or a terminal chat's in the drawer); aurora(null): stop.
function aurora(canvas) {
  if (canvas === AU.canvas) return;
  cancelAnimationFrame(AU.raf);
  AU.raf = 0;
  AU.ro?.disconnect();
  AU.gl = null;
  AU.canvas = canvas;
  const x = canvas && auroraGL(canvas);
  if (!x) return;
  const { gl } = x;
  AU.gl = gl;
  AU.u = x.u;
  const fit = () => { // half the device pixels: soft by nature, and a quarter of the work
    const k = Math.min(devicePixelRatio || 1, 2) * 0.5;
    canvas.width = Math.max(1, Math.round(canvas.clientWidth * k));
    canvas.height = Math.max(1, Math.round(canvas.clientHeight * k));
    gl.viewport(0, 0, canvas.width, canvas.height);
    if (calm.matches) auroraDraw(); // a still aurora redraws only when its size changes
  };
  AU.ro = new ResizeObserver(fit);
  AU.ro.observe(canvas);
  fit();
  AU.last = performance.now();
  auroraLoop();
}

// auroraEnergy(busy): where the energy heads; the frames ease it there (about 1.5 s each way).
function auroraEnergy(busy) { AU.goal = busy ? 1 : 0; }

function auroraDraw() {
  const { gl, u, canvas } = AU;
  gl.clearColor(0, 0, 0, 0);
  gl.clear(gl.COLOR_BUFFER_BIT);
  gl.uniform2f(u.R, canvas.width, canvas.height);
  gl.uniform1f(u.T, AU.t);
  gl.uniform1f(u.E, AU.e);
  gl.drawArrays(gl.TRIANGLES, 0, 3);
}

function auroraLoop() {
  AU.raf = 0;
  if (!AU.gl || !AU.canvas?.isConnected || document.hidden) return;
  const now = performance.now();
  const dt = Math.min(0.05, (now - AU.last) / 1000);
  AU.last = now;
  AU.e += (AU.goal - AU.e) * (1 - Math.exp(-dt * 2.2));
  AU.t += dt * (0.55 + 2.4 * AU.e * AU.e); // calm drift, a steady surge while working
  auroraDraw();
  if (!calm.matches) AU.raf = requestAnimationFrame(auroraLoop);
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && AU.canvas && !AU.raf) { AU.last = performance.now(); auroraLoop(); }
});
