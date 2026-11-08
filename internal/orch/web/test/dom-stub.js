// A minimal DOM, just enough for el(), the renderers and poll(). It never
// parses HTML: there is deliberately no innerHTML, so a test that observes a
// child element where text was expected has found a real bug.

export class Node {
  constructor(nodeType) {
    this.nodeType = nodeType;
    this.childNodes = [];
    this.parentNode = null;
  }
  get firstChild() { return this.childNodes[0] || null; }
  appendChild(c) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    this.childNodes.push(c);
    return c;
  }
  append(...cs) {
    for (const c of cs) this.appendChild(typeof c === 'object' ? c : new Text(String(c)));
  }
  removeChild(c) {
    const i = this.childNodes.indexOf(c);
    if (i >= 0) this.childNodes.splice(i, 1);
    c.parentNode = null;
    return c;
  }
}

export class Text extends Node {
  constructor(data) { super(3); this.data = String(data); }
  get textContent() { return this.data; }
}

export class Element extends Node {
  constructor(tag) {
    super(1);
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.listeners = {};
  }
  get children() { return this.childNodes.filter((c) => c.nodeType === 1); }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  hasAttribute(k) { return this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); }
  addEventListener(ev, fn) { (this.listeners[ev] ||= []).push(fn); }
  get className() { return this.getAttribute('class') || ''; }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
}

export class Document extends EventTarget {
  constructor() {
    super();
    this.visibilityState = 'visible';
  }
  createElement(tag) { return new Element(tag); }
  createTextNode(s) { return new Text(s); }
  setVisibility(v) {
    this.visibilityState = v;
    this.dispatchEvent(new Event('visibilitychange'));
  }
}

export function installDocument() {
  const d = new Document();
  globalThis.document = d;
  return d;
}

export function findAll(root, pred, out = []) {
  if (root.nodeType === 1 && pred(root)) out.push(root);
  for (const c of root.childNodes) findAll(c, pred, out);
  return out;
}
export const byClass = (root, cls) => findAll(root, (e) => e.className.split(/\s+/).includes(cls));
export const allElements = (root) => findAll(root, () => true);
