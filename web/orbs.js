'use strict';
// Status orbs: a small glass sphere per terminal whose colour and motion say its state, with no words.
// The look is a WebGL port of Murmur's glass body and its "duet" interior (github.com/krispuckett/murmur,
// MIT licence, (c) Kris Puckett): an SDF sphere with a fresnel rim, two specular lobes, a soft contact glow, a
// faint haze sampled at five depths along the refracted ray, and two lights orbiting inside it.
//
// One WebGL canvas, off the page, draws each state once per frame and every <canvas class="orbc" data-o=…>
// copies it: a page shows dozens of orbs, and a browser allows only a handful of WebGL contexts. Only the
// orbs on screen are drawn, 30 times a second, and none while the tab is hidden. Without WebGL (or with
// reduced motion) each orb is drawn once and stays still; with no WebGL at all, CSS paints a plain ball.
const ORB_STATES = {
  // agents
  working: { label: 'Working', tone: '#4f8dff', tone2: '#9b7dff', speed: 1.45, glow: 1.3, drive: 1, complete: 0, sep: 1, lights: 1 },
  input: { label: 'Input', tone: '#ff9f43', tone2: '#ffcf70', speed: 0.9, glow: 1.15, drive: 0, complete: 0, sep: 1, lights: 1, breathe: [0.22, 1.8] },
  blocked: { label: 'Blocked', tone: '#ffd23f', tone2: '#ffa62b', speed: 0.5, glow: 1.2, drive: 0, complete: 0, sep: 1.15, lights: 1, breathe: [0.4, 2.4] },
  done: { label: 'Done', tone: '#2fe39a', tone2: '#5ad1ff', speed: 0.45, glow: 1.05, drive: 0, complete: 1, sep: 1, lights: 1 },
  // servers
  running: { label: 'Running', tone: '#a77bff', tone2: '#6f8cff', speed: 0.55, glow: 1.0, drive: 0.25, complete: 0, sep: 1, lights: 1 },
  slow: { label: 'Slow', tone: '#ff9f43', tone2: '#ff7a3d', speed: 0.22, glow: 0.95, drive: 0, complete: 0, sep: 1.1, lights: 1, breathe: [0.15, 3.5] },
  stopped: { label: 'Stopped', tone: '#8b949e', tone2: '#8b949e', speed: 0, glow: 0.5, drive: 0, complete: 0, sep: 1, lights: 0 },
  error: { label: 'Error', tone: '#ff4d4d', tone2: '#ff8a3d', speed: 0.65, glow: 0.95, drive: 0, complete: 0, sep: 1, lights: 1, stutter: true },
  // shells
  idle: { label: 'Idle', tone: '#7d8794', tone2: '#7d8794', speed: 0.18, glow: 0.6, drive: 0, complete: 0, sep: 1, lights: 0.5 },
};

// orbHTML: the markup of one orb, `size` CSS pixels across (a class, .o10 to .o30: no inline styles); the state's
// name is its accessible label. No width attribute: the engine sizes the canvas, and a re-render must not reset it.
const orbHTML = (k, size = 20, tip = '') =>
  `<canvas class="orbc o${size}" data-o="${k}" role="img" aria-label="${ORB_STATES[k]?.label || k}" title="${tip || ORB_STATES[k]?.label || k}"></canvas>`;

(() => {
  const SIZE = 128;
  let cv, gl, U; // the engine, made when the first orb comes on the page (not at boot: most views have none)
  const noGL = () => { document.documentElement.classList.add('nogl'); return false; };
  function init() {
    cv = document.createElement('canvas');
    cv.width = cv.height = SIZE;
    gl = cv.getContext('webgl', { premultipliedAlpha: true, alpha: true, preserveDrawingBuffer: true, antialias: false });
    if (!gl) return noGL();
    const VS = 'attribute vec2 a; varying vec2 vP; void main() { vP = a; gl_Position = vec4(a, 0.0, 1.0); }';
    const FS = `precision highp float;
varying vec2 vP;
uniform float uT, uGlow, uDrive, uComplete, uSep, uLights;
uniform vec3 uTone, uTone2;
const float R = 0.74;   // the body's radius in the canvas, leaving room for the glow
const float EXT = 0.45; // how fast light dies going deeper into the glass
float h3(vec3 p) { p = fract(p * 0.3183099 + 0.1); p *= 17.0; return fract(p.x * p.y * p.z * (p.x + p.y + p.z)); }
float n3(vec3 x) {
  vec3 i = floor(x), f = fract(x); f = f * f * (3.0 - 2.0 * f);
  return mix(mix(mix(h3(i), h3(i + vec3(1, 0, 0)), f.x), mix(h3(i + vec3(0, 1, 0)), h3(i + vec3(1, 1, 0)), f.x), f.y),
             mix(mix(h3(i + vec3(0, 0, 1)), h3(i + vec3(1, 0, 1)), f.x), mix(h3(i + vec3(0, 1, 1)), h3(i + vec3(1, 1, 1)), f.x), f.y), f.z) * 2.0 - 1.0;
}
float inside(vec3 p) { return 1.0 - smoothstep(0.76, 0.99, length(p)); }
vec3 rotY(vec3 p, float a) { float c = cos(a), s = sin(a); return vec3(c * p.x + s * p.z, p.y, -s * p.x + c * p.z); }
void main() {
  vec2 s = vec2(vP.x, -vP.y) / R; // y runs down, as in Murmur: the key light sits up and to the left
  float t = uT;
  float rho = length(s);
  float Rd = 1.0 + 0.018 * sin(t * 1.1) * uLights;
  float z = sqrt(max(Rd * Rd - rho * rho, 0.0));
  vec3 P = vec3(s, z);
  vec3 N = normalize(P + vec3(0.0, 0.0, 1e-6));
  float m = 1.0 - smoothstep(Rd - 0.035, Rd + 0.035, rho);
  float fres = 1.0 - clamp(N.z, 0.0, 1.0);
  // the bend at entry, and how far the ray runs inside
  vec3 V = vec3(0.0, 0.0, -1.0);
  float ci = clamp(-dot(V, N), 0.0, 1.0), eta = 0.75, k = 1.0 - eta * eta * (1.0 - ci * ci);
  vec3 rd = k <= 0.0 ? V : normalize(eta * V + (eta * ci - sqrt(k)) * N);
  float b = dot(P, rd), c = dot(P, P) - 1.0, disc = b * b - c;
  float L = disc > 0.0 ? clamp(-b + sqrt(disc), 0.0, 2.2) : 0.0;
  // the duet: two lights orbiting on a slowly precessing plane, braiding when the agent works
  float lean = 0.62 + 0.20 * sin(t * 0.037);
  float prec = t * 0.064 + 0.45 * sin(t * 0.045 + 2.0);
  vec3 e1 = rotY(vec3(1.0, 0.0, 0.0), prec), e2 = rotY(vec3(0.0, sin(lean), cos(lean)), prec), nrm = cross(e1, e2);
  float r = 0.40 * uSep * (1.0 - 0.34 * uDrive) * (1.0 - 0.62 * uComplete);
  float rate = 0.675 * (1.0 + 0.9 * uDrive);
  float psi = t * rate + 0.4 * sin(t * rate * 0.5 + 3.0);
  float braid = 0.16 * uDrive * sin(psi * 3.0);
  vec3 spoke = cos(psi) * e1 + sin(psi) * e2;
  vec3 A = r * spoke + nrm * braid, B = -r * spoke - nrm * braid;
  float wA = 0.21, wB = wA * 0.8; // a touch larger than Murmur's: these orbs live at 14-28 px
  float bal = clamp(0.5 + 0.15 * sin(t * 0.21 + 0.5 * sin(t * 0.11 + 7.0)), 0.06, 0.94);
  vec3 toA = A - P; float sA = dot(toA, rd); float argA = max(dot(toA, toA) - sA * sA, 0.0) / (wA * wA);
  float visA = (sA > 0.0 && sA < L) ? inside(P + rd * sA) * exp(-EXT * sA) : 0.0;
  float coreA = exp(-argA) * visA;
  vec3 toB = B - P; float sB = dot(toB, rd); float argB = max(dot(toB, toB) - sB * sB, 0.0) / (wB * wB);
  float visB = (sB > 0.0 && sB < L) ? inside(P + rd * sB) * exp(-EXT * sB) : 0.0;
  float coreB = exp(-argB) * visB;
  float occA = 1.0, occB = 1.0;
  if (sA < sB) { occB = exp(-2.4 * coreA); } else { occA = exp(-2.4 * coreB); }
  float flare = 1.0 + 0.6 * uComplete;
  float eA = (coreA * 1.05 + 0.30 * exp(-argA * 0.098) * visA) * 2.5 * bal * occA * flare * uLights;
  float eB = (coreB * 1.05 + 0.30 * exp(-argB * 0.098) * visB) * 2.5 * (1.0 - bal) * occB * flare * uLights;
  // the medium: a faint haze through the middle, so the glass reads full rather than hollow
  float acc = 0.0, trans = 1.0, ds = L / 5.0;
  for (int i = 0; i < 5; i++) {
    vec3 p = P + rd * ((float(i) + 0.5) * ds);
    float fog = 1.0 - smoothstep(0.05, 0.98, length(p));
    float hz = clamp(0.5 + 0.85 * n3(p * 2.2 + vec3(t * 0.051, -t * 0.033, t * 0.089)), 0.0, 1.0);
    float med = fog * (0.55 + 0.45 * hz) * 0.085;
    acc += med * trans * ds;
    trans *= exp(-(2.2 * med + EXT) * ds);
  }
  float transmit = 1.0 - 0.88 * pow(fres, 2.2);
  float interior = (acc * 3.6 + eA + eB) * m * transmit;
  // the surface: two specular lobes, a rim brighter away from the key, a contact glow pooling below
  vec3 key = normalize(vec3(-0.52 + 0.055 * sin(t * 0.21), -0.60 + 0.045 * cos(t * 0.174), 0.61));
  vec3 H = normalize(key + vec3(0.0, 0.0, 1.0));
  float nh = clamp(dot(N, H), 0.0, 1.0);
  float spec = (pow(nh, 40.0) + 0.09 * pow(nh, 4.0)) * m * 0.42;
  float wrap = 0.55 + 0.45 * clamp(dot(normalize(N.xy + 1e-4), normalize(vec2(0.42, 0.50))), 0.0, 1.0);
  float rim = pow(fres, 3.9) * m * wrap * 0.9 * mix(0.86, 1.14, 0.5 - 0.5 * N.y);
  float outr = (rho - Rd) / 0.13;
  float glowO = exp(-outr * outr) * (1.0 - m) * (0.55 + 0.55 * smoothstep(-0.25, 0.85, s.y)) * 0.16 * uGlow;
  float e = (interior + rim) * uGlow;
  float hueW = (eA * 0.85 - eB) / max(eA + eB, 1e-4);
  vec3 tone = mix(uTone, uTone2, clamp(0.5 - 0.5 * hueW, 0.0, 1.0) * 0.6);
  vec3 col = tone * (1.0 - exp(-e * 1.9)) * 1.15 + vec3(1.0, 0.96, 0.9) * smoothstep(0.7, 1.9, e) * 0.5
           + vec3(1.0, 0.98, 0.95) * spec * 1.4 + vec3(0.012, 0.016, 0.026);
  float gA = clamp(glowO * 1.8, 0.0, 1.0);
  float a = clamp(m + (1.0 - m) * gA, 0.0, 1.0);
  vec3 straight = a > 0.0 ? (col * m + tone * gA * (1.0 - m)) / a : vec3(0.0);
  straight = pow(clamp(straight, 0.0, 1.0), vec3(1.0 / 2.2));
  gl_FragColor = vec4(straight * a, a);
}`;
    const sh = (type, src) => { const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s); return gl.getShaderParameter(s, gl.COMPILE_STATUS) ? s : null; };
    const vs = sh(gl.VERTEX_SHADER, VS), fs = sh(gl.FRAGMENT_SHADER, FS);
    if (!vs || !fs) return noGL();
    const pr = gl.createProgram();
    gl.attachShader(pr, vs); gl.attachShader(pr, fs); gl.linkProgram(pr);
    if (!gl.getProgramParameter(pr, gl.LINK_STATUS)) return noGL();
    gl.useProgram(pr);
    gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
    const loc = gl.getAttribLocation(pr, 'a');
    gl.enableVertexAttribArray(loc);
    gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
    gl.viewport(0, 0, SIZE, SIZE);
    U = {};
    for (const n of ['uT', 'uGlow', 'uDrive', 'uComplete', 'uSep', 'uLights', 'uTone', 'uTone2']) U[n] = gl.getUniformLocation(pr, n);
    const lin = (hex) => [1, 3, 5].map((i) => Math.pow(parseInt(hex.slice(i, i + 2), 16) / 255, 2.2));
    for (const o of Object.values(ORB_STATES)) { o.lt = lin(o.tone); o.lt2 = lin(o.tone2); }
    return true;
  }

  const phase = {}, seen = new WeakSet(), vis = new Set(), all = document.getElementsByClassName('orbc');
  const io = new IntersectionObserver((es) => { for (const e of es) (e.isIntersecting ? vis.add(e.target) : vis.delete(e.target)); });
  const still = matchMedia('(prefers-reduced-motion: reduce)');
  let prev = 0, last = 0, on = false, ok;
  function frame(now) {
    if (!all.length) { on = false; return; } // no orb left on the page: the loop sleeps until one comes (wake)
    requestAnimationFrame(frame);
    for (const c of all) if (!seen.has(c)) { seen.add(c); io.observe(c); }
    if (document.hidden || now - last < 33) return;
    const dt = Math.min(0.1, (now - (prev || now)) / 1000);
    prev = last = now;
    const groups = {};
    for (const c of vis) {
      if (!c.isConnected) { vis.delete(c); continue; }
      if (still.matches && c.drawn === c.dataset.o) continue; // still: drawn once, then left alone
      (groups[c.dataset.o] ||= []).push(c);
    }
    for (const k in groups) {
      const o = ORB_STATES[k];
      if (!o) continue;
      phase[k] = (phase[k] ?? 7 + Math.random() * 40) + (still.matches ? 0 : dt * o.speed);
      let g = o.glow;
      const s = now / 1000;
      if (!still.matches && o.breathe) g *= 1 + o.breathe[0] * Math.sin((s * 2 * Math.PI) / o.breathe[1]);
      if (!still.matches && o.stutter) { const f = s % 2.6; g *= f < 0.12 ? 0.55 : f < 0.2 ? 1.15 : f < 0.28 ? 0.7 : 1; }
      gl.uniform1f(U.uT, phase[k]); gl.uniform1f(U.uGlow, g); gl.uniform1f(U.uDrive, o.drive); gl.uniform1f(U.uComplete, o.complete);
      gl.uniform1f(U.uSep, o.sep); gl.uniform1f(U.uLights, o.lights); gl.uniform3fv(U.uTone, o.lt); gl.uniform3fv(U.uTone2, o.lt2);
      gl.clearColor(0, 0, 0, 0); gl.clear(gl.COLOR_BUFFER_BIT);
      gl.drawArrays(gl.TRIANGLES, 0, 3);
      for (const c of groups[k]) {
        const px = Math.round((c.clientWidth || 16) * Math.min(4, (devicePixelRatio || 1) * 1.5));
        if (c.width !== px) c.width = c.height = px;
        const x = c.getContext('2d');
        x.clearRect(0, 0, px, px);
        x.drawImage(cv, 0, 0, px, px);
        c.drawn = k;
      }
    }
  }
  // the loop runs only while an orb is on the page (an idle one cost a phone ~40 ms of main thread a second)
  const wake = () => {
    if (on || !all.length || !(ok ??= init())) return; // no WebGL: CSS paints them
    on = true;
    prev = 0;
    requestAnimationFrame(frame);
  };
  new MutationObserver(wake).observe(document.documentElement, { childList: true, subtree: true });
  wake();
})();
