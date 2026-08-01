#include "shim.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Implemented in Go (pipewire.go). Called on the loop thread; they enqueue and
 * return, never call back into pipewire. */
extern void gawkOnGlobal(unsigned int id, int kind, const struct spa_dict *props);
extern void gawkOnObjectProps(unsigned int id, int kind, const struct spa_dict *props);
extern void gawkOnGlobalRemove(unsigned int id);
extern void gawkOnCoreDone(int seq);
extern void gawkOnCoreError(unsigned int id, int res, const char *message);

/*
 * Bound objects.
 *
 * Measured, not assumed (2026-08-01, against PipeWire 1.0.5): the registry's
 * *global* properties do NOT carry `application.process.binary`. A Client's
 * globals stop at `application.name`; the binary — which is the identity R35
 * links against (AD2) — appears only in the properties a **bound** object
 * reports through its info event. So every Client, and every audio-output
 * Stream node, is bound and its info folded over the global's properties.
 *
 * The cost is a proxy per audio-relevant object, which on a busy desktop is
 * tens of them. Binding *everything* would be simpler and is deliberately not
 * done: the graph would grow proxies for every port on the machine to learn
 * nothing.
 */
struct bound {
	struct spa_hook obj_listener;
	struct spa_hook proxy_listener;
	struct pw_proxy *proxy;
	uint32_t id;
	int kind;
};

static void on_client_info(void *data, const struct pw_client_info *info)
{
	struct bound *b = data;
	if (info != NULL)
		gawkOnObjectProps(b->id, GAWK_KIND_CLIENT, info->props);
}

static const struct pw_client_events client_events = {
	PW_VERSION_CLIENT_EVENTS,
	.info = on_client_info,
};

static void on_node_info(void *data, const struct pw_node_info *info)
{
	struct bound *b = data;
	if (info != NULL)
		gawkOnObjectProps(b->id, GAWK_KIND_NODE, info->props);
}

static const struct pw_node_events node_events = {
	PW_VERSION_NODE_EVENTS,
	.info = on_node_info,
};

/* The object went away: drop our proxy so it does not outlive it. */
static void on_bound_removed(void *data)
{
	struct bound *b = data;
	pw_proxy_destroy(b->proxy);
}

static void on_bound_destroy(void *data)
{
	struct bound *b = data;
	spa_hook_remove(&b->obj_listener);
	spa_hook_remove(&b->proxy_listener);
	b->proxy = NULL;
}

static const struct pw_proxy_events bound_proxy_events = {
	PW_VERSION_PROXY_EVENTS,
	.removed = on_bound_removed,
	.destroy = on_bound_destroy,
};

/* Binds one global so its full property list reaches us. */
static void bind_object(struct gawk_pw *pw, uint32_t id, const char *type,
                        uint32_t version, int kind)
{
	struct pw_proxy *proxy = pw_registry_bind(pw->registry, id, type, version,
	                                          sizeof(struct bound));
	if (proxy == NULL)
		return;
	struct bound *b = pw_proxy_get_user_data(proxy);
	b->proxy = proxy;
	b->id = id;
	b->kind = kind;
	if (kind == GAWK_KIND_CLIENT)
		pw_client_add_listener((struct pw_client *)proxy, &b->obj_listener,
		                       &client_events, b);
	else
		pw_node_add_listener((struct pw_node *)proxy, &b->obj_listener,
		                     &node_events, b);
	pw_proxy_add_listener(proxy, &b->proxy_listener, &bound_proxy_events, b);
}

static int kind_of(const char *type)
{
	if (type == NULL)
		return GAWK_KIND_OTHER;
	if (strcmp(type, PW_TYPE_INTERFACE_Node) == 0)
		return GAWK_KIND_NODE;
	if (strcmp(type, PW_TYPE_INTERFACE_Port) == 0)
		return GAWK_KIND_PORT;
	if (strcmp(type, PW_TYPE_INTERFACE_Client) == 0)
		return GAWK_KIND_CLIENT;
	return GAWK_KIND_OTHER;
}

static void on_global(void *data, uint32_t id, uint32_t permissions,
                      const char *type, uint32_t version,
                      const struct spa_dict *props)
{
	struct gawk_pw *pw = data;
	(void)permissions;
	(void)version;
	int kind = kind_of(type);
	if (kind == GAWK_KIND_OTHER)
		return;
	gawkOnGlobal(id, kind, props);

	/* Bind what we need the full property list of — see struct bound. */
	if (kind == GAWK_KIND_CLIENT) {
		bind_object(pw, id, type, PW_VERSION_CLIENT, kind);
		return;
	}
	if (kind == GAWK_KIND_NODE && props != NULL) {
		const char *class = spa_dict_lookup(props, PW_KEY_MEDIA_CLASS);
		if (class != NULL && strcmp(class, "Stream/Output/Audio") == 0)
			bind_object(pw, id, type, PW_VERSION_NODE, kind);
	}
}

static void on_global_remove(void *data, uint32_t id)
{
	(void)data;
	gawkOnGlobalRemove(id);
}

static const struct pw_registry_events registry_events = {
	PW_VERSION_REGISTRY_EVENTS,
	.global = on_global,
	.global_remove = on_global_remove,
};

static void on_core_done(void *data, uint32_t id, int seq)
{
	(void)data;
	if (id == PW_ID_CORE)
		gawkOnCoreDone(seq);
}

static void on_core_error(void *data, uint32_t id, int seq, int res,
                          const char *message)
{
	(void)data;
	(void)seq;
	gawkOnCoreError(id, res, message);
}

static const struct pw_core_events core_events = {
	PW_VERSION_CORE_EVENTS,
	.done = on_core_done,
	.error = on_core_error,
};

static char *dupf(const char *fmt, const char *detail)
{
	char *buf = malloc(512);
	if (buf == NULL)
		return NULL;
	snprintf(buf, 512, fmt, detail ? detail : "");
	return buf;
}

struct gawk_pw *gawk_pw_new(char **err)
{
	static int inited = 0;
	if (!inited) {
		pw_init(NULL, NULL);
		inited = 1;
	}

	struct gawk_pw *pw = calloc(1, sizeof(*pw));
	if (pw == NULL) {
		*err = dupf("out of memory%s", "");
		return NULL;
	}

	pw->loop = pw_thread_loop_new("gawk-pw-helper", NULL);
	if (pw->loop == NULL) {
		*err = dupf("could not create the PipeWire loop%s", "");
		goto fail;
	}
	pw->context = pw_context_new(pw_thread_loop_get_loop(pw->loop), NULL, 0);
	if (pw->context == NULL) {
		*err = dupf("could not create the PipeWire context%s", "");
		goto fail;
	}
	/* No object.linger anywhere: every object we create belongs to this
	 * connection, so the daemon reaps it when we go away — however we go
	 * away. */
	pw->core = pw_context_connect(pw->context, NULL, 0);
	if (pw->core == NULL) {
		*err = dupf("could not connect to the PipeWire daemon (is it running?)%s", "");
		goto fail;
	}
	pw_core_add_listener(pw->core, &pw->core_listener, &core_events, pw);

	pw->registry = pw_core_get_registry(pw->core, PW_VERSION_REGISTRY, 0);
	if (pw->registry == NULL) {
		*err = dupf("could not get the PipeWire registry%s", "");
		goto fail;
	}
	pw_registry_add_listener(pw->registry, &pw->registry_listener,
	                         &registry_events, pw);

	if (pw_thread_loop_start(pw->loop) < 0) {
		*err = dupf("could not start the PipeWire loop%s", "");
		goto fail;
	}
	pw->started = 1;
	return pw;

fail:
	gawk_pw_free(pw);
	return NULL;
}

void gawk_pw_free(struct gawk_pw *pw)
{
	if (pw == NULL)
		return;
	if (pw->loop != NULL && pw->started)
		pw_thread_loop_stop(pw->loop);
	if (pw->registry != NULL)
		pw_proxy_destroy((struct pw_proxy *)pw->registry);
	if (pw->core != NULL)
		pw_core_disconnect(pw->core);
	if (pw->context != NULL)
		pw_context_destroy(pw->context);
	if (pw->loop != NULL)
		pw_thread_loop_destroy(pw->loop);
	free(pw);
}

void gawk_pw_lock(struct gawk_pw *pw) { pw_thread_loop_lock(pw->loop); }
void gawk_pw_unlock(struct gawk_pw *pw) { pw_thread_loop_unlock(pw->loop); }

int gawk_pw_sync(struct gawk_pw *pw)
{
	return pw_core_sync(pw->core, PW_ID_CORE, 0);
}

struct pw_proxy *gawk_pw_create_sink(struct gawk_pw *pw, const char *name,
                                     const char *desc, const char *positions,
                                     int channels)
{
	char chans[16];
	char pos[256];
	snprintf(chans, sizeof(chans), "%d", channels);
	snprintf(pos, sizeof(pos), "[ %s ]", positions);

	struct pw_properties *props = pw_properties_new(
		/* The adapter factory wraps this SPA factory; the pair is what
		 * `pw-cli create-node adapter` does, and what OBS's audio capture
		 * plugin does. */
		"factory.name", "support.null-audio-sink",
		"node.name", name,
		"node.description", desc,
		/* Audio/Sink/**Internal**: a real sink for linking purposes, but
		 * hidden from the device lists applications and volume controls
		 * show. A user must never find a "gawk" output device and wonder
		 * whether to select it. */
		"media.class", "Audio/Sink/Internal",
		"audio.position", pos,
		"audio.channels", chans,
		"monitor.channel-volumes", "true",
		/* Deliberately absent: object.linger. Its absence *is* the crash
		 * safety (docs/39 D4). */
		"node.virtual", "true",
		NULL);
	if (props == NULL)
		return NULL;

	struct pw_proxy *proxy = pw_core_create_object(
		pw->core, "adapter", PW_TYPE_INTERFACE_Node, PW_VERSION_NODE,
		&props->dict, 0);
	pw_properties_free(props);
	return proxy;
}

struct pw_proxy *gawk_pw_create_link(struct gawk_pw *pw, uint32_t out_node,
                                     uint32_t out_port, uint32_t in_node,
                                     uint32_t in_port)
{
	char on[16], op[16], in[16], ip[16];
	snprintf(on, sizeof(on), "%u", out_node);
	snprintf(op, sizeof(op), "%u", out_port);
	snprintf(in, sizeof(in), "%u", in_node);
	snprintf(ip, sizeof(ip), "%u", in_port);

	struct pw_properties *props = pw_properties_new(
		PW_KEY_LINK_OUTPUT_NODE, on,
		PW_KEY_LINK_OUTPUT_PORT, op,
		PW_KEY_LINK_INPUT_NODE, in,
		PW_KEY_LINK_INPUT_PORT, ip,
		/* Again no linger: a link outliving us would be a routing change
		 * we made to someone's machine and did not undo. */
		NULL);
	if (props == NULL)
		return NULL;

	struct pw_proxy *proxy = pw_core_create_object(
		pw->core, "link-factory", PW_TYPE_INTERFACE_Link, PW_VERSION_LINK,
		&props->dict, 0);
	pw_properties_free(props);
	return proxy;
}

void gawk_pw_destroy_proxy(struct pw_proxy *proxy)
{
	if (proxy != NULL)
		pw_proxy_destroy(proxy);
}

unsigned gawk_dict_n(const struct spa_dict *d)
{
	return d == NULL ? 0 : d->n_items;
}

const char *gawk_dict_key(const struct spa_dict *d, unsigned i)
{
	if (d == NULL || i >= d->n_items)
		return NULL;
	return d->items[i].key;
}

const char *gawk_dict_value(const struct spa_dict *d, unsigned i)
{
	if (d == NULL || i >= d->n_items)
		return NULL;
	return d->items[i].value;
}

const char *gawk_pw_version(void) { return pw_get_library_version(); }
