# alert-renderer: readable Discord alerts

[`royal-road/alert-renderer`](../../alert-renderer/README.md) receives
vmalertmanager's webhook. It posts each notification to Discord with:
- the summary and the identifying labels;
- a chart of the rule with its threshold;
- a link to the workload's Observe tab.

It replaces the `discord_configs` receiver.

```
vmalertmanager ─(webhook)─▶ alert-renderer ─▶ vmalert   (the rule's expression)
                                           ├▶ vmselect  (query_range for the chart)
                                           └▶ Discord   (webhook, HTTPS)
```

| File | What |
|---|---|
| `10-alert-renderer.yaml` | Deployment and ClusterIP Service in `openshift-monitoring`. Reads Secret `alertmanager-discord-webhook` (key `url`), which vmalertmanager already uses. |
| `20-networkpolicy.yaml` | Ingress only from vmalertmanager. Egress only to vmalert, vmselect, DNS, and HTTPS outside private ranges. |

The pod:
- runs as UID 65532 with a read-only root filesystem;
- has no service account token and drops all capabilities;
- uses seccomp `RuntimeDefault`;
- has 10m/16Mi requests and 500m/128Mi limits.

It passes the `restricted` Pod Security level.

## Install

1. Replace `<sha>` with the image tag CI pushed
   (`ghcr.io/royal-road/alert-renderer`, short SHA of main) and
   `<console-base-address>` with the console Deployment's `BRIDGE_BASE_ADDRESS`
   (`kubectl -n openshift-console get deploy openshift-console -o yaml`). If
   the console has several host names, use that one, so links in Discord land
   on the host that logins return to. Then apply both files:

   ```sh
   kubectl apply -f royal-road/deploy/alert-renderer/
   kubectl -n openshift-monitoring rollout status deploy/alert-renderer
   ```

2. Try it before switching: port-forward and post one of
   `royal-road/alert-renderer/testdata/payloads/*.json`. This posts a real
   message to the channel.

   ```sh
   kubectl -n openshift-monitoring port-forward svc/alert-renderer 8080 &
   curl --data-binary @royal-road/alert-renderer/testdata/payloads/ingress-5xx-firing.json \
     http://127.0.0.1:8080/alertmanager
   ```

3. Switch the receiver. In the infrastructure repository's
   `observability/victoria-metrics/vmalertmanager-config.patch.yaml`, replace
   the `discord_webhook` receiver's `discord_configs` with:

   ```yaml
   - name: discord_webhook
     webhook_configs:
       - url: http://alert-renderer.openshift-monitoring.svc:8080/alertmanager
         send_resolved: true
         max_alerts: 30
         timeout: 30s
   ```

   Keep `alertmanager-discord-webhook` in `spec.secrets`; alert-renderer
   mounts the same Secret. Apply the patch as that file's header describes.

4. Check the next notification in Discord and the pod log. There is one JSON
   line per notification, with `charts` and, on failure, `error`.

**Rollback:** restore `discord_configs` in the patch and re-apply it. alert-renderer can stay deployed; nothing calls it.

## Notes

- Discord's IP ranges are not fixed, so the HTTPS egress rule allows any
  public address on port 443.
- vmalert's own `-external.url` and source template are not used. The link
  comes from the alert's labels, so the vmalert pod IP never reaches Discord.
- Links without a workload label go to the namespace's Observe tab, or to
  Observe › Overview when there is no namespace (see the alert-renderer
  README, "Links"). Every link target exists in observe-plugin today.
