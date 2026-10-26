// A UDP load balancer stand-in for the R79 voice e2e (docs/80 MV7): the one
// UDP LoadBalancer Service a bridge fleet sits behind (D1, OD22), reduced to
// the two properties the drain test depends on.
//
//  * A FLOW (client address:port) is pinned to the backend it first reached,
//    for as long as it keeps sending — kube-proxy's UDP conntrack entry. A
//    QUIC connection is one flow, so a session never changes pods mid-life.
//  * A backend marked TERMINATING receives no NEW flows, while its existing
//    flows keep flowing to it — a terminating endpoint since Kubernetes 1.26.
//    That is what lets a draining bridge send its 4002 close to browsers that
//    are still connected to it (the docs/22 lesson), while the rejoin that
//    follows lands on another pod.
//
// It is not a general proxy: no health checks of its own (the harness says
// which backend is terminating), one listening socket per frontend, and an
// idle flow is forgotten after FLOW_IDLE_MS.

import dgram from 'node:dgram';

const FLOW_IDLE_MS = 60_000;

export class UdpBalancer {
  /**
   * @param {{listen: number, host?: string, backends: {name: string, port: number}[]}} opts
   */
  constructor({ listen, host = '127.0.0.1', backends }) {
    this.listenPort = listen;
    this.host = host;
    this.backends = backends.map((b) => ({ ...b, terminating: false, flows: 0 }));
    this.flows = new Map(); // "addr:port" -> {sock, backend, last}
    this.next = 0;
    this.front = dgram.createSocket('udp4');
    this.front.on('message', (msg, rinfo) => this.fromClient(msg, rinfo));
    this.sweep = setInterval(() => this.expire(), 5_000);
    this.sweep.unref();
  }

  start() {
    return new Promise((resolve, reject) => {
      this.front.once('error', reject);
      this.front.bind(this.listenPort, this.host, () => {
        this.front.off('error', reject);
        resolve();
      });
    });
  }

  /** New flows stop reaching `name`; its existing flows are kept. */
  terminate(name) {
    for (const b of this.backends) if (b.name === name) b.terminating = true;
  }

  /** Flow counts per backend, for assertions. */
  stats() {
    return Object.fromEntries(this.backends.map((b) => [b.name, b.flows]));
  }

  pick() {
    const live = this.backends.filter((b) => !b.terminating);
    if (live.length === 0) return null;
    const b = live[this.next % live.length];
    this.next++;
    return b;
  }

  fromClient(msg, rinfo) {
    const key = `${rinfo.address}:${rinfo.port}`;
    let flow = this.flows.get(key);
    if (!flow) {
      const backend = this.pick();
      if (!backend) return;
      const sock = dgram.createSocket('udp4');
      sock.on('message', (reply) => this.front.send(reply, rinfo.port, rinfo.address));
      sock.on('error', () => {});
      sock.bind(0, this.host);
      flow = { sock, backend, last: Date.now() };
      backend.flows++;
      this.flows.set(key, flow);
    }
    flow.last = Date.now();
    flow.sock.send(msg, flow.backend.port, this.host);
  }

  expire() {
    const cutoff = Date.now() - FLOW_IDLE_MS;
    for (const [key, flow] of this.flows) {
      if (flow.last < cutoff) {
        flow.sock.close();
        flow.backend.flows--;
        this.flows.delete(key);
      }
    }
  }

  close() {
    clearInterval(this.sweep);
    for (const flow of this.flows.values()) flow.sock.close();
    this.flows.clear();
    this.front.close();
  }
}
