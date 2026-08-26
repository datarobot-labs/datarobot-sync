+++
title = 'dr-sync'
layout = 'hextra-home'
+++

{{< hextra/hero-badge >}}
  <div class="hx:w-2 hx:h-2 hx:rounded-full hx:bg-primary-400"></div>
  <span>Also a `dr` CLI plugin</span>
{{< /hextra/hero-badge >}}
{{< hextra/hero-badge >}}
  <div class="hx:w-2 hx:h-2 hx:rounded-full hx:bg-primary-400"></div>
  <span>Works with DataRobot 11.4+</span>
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-6">
{{< hextra/hero-headline >}}
  rsync for the&nbsp;<br class="hx:sm:block hx:hidden" />DataRobot Files API
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-subtitle >}}
  A single static Go binary that makes a local directory and a DataRobot Files&nbsp;<br class="hx:sm:block hx:hidden" />container match, transferring only the difference, xz-compressed
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-button text="Read the docs" link="docs" >}}
</div>

<div class="hx:mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Nothing unchanged is re-sent"
    subtitle="The Files API records a SHA-256 for every stored object, so the diff is exact rather than heuristic."
  >}}
  {{< hextra/feature-card
    title="Renames and reverts are free"
    subtitle="Content is stored under its own digest, so moving a 5 GB file is a manifest edit, not a transfer."
  >}}
  {{< hextra/feature-card
    title="No-op runs stay fast"
    subtitle="The remote manifest records size, mode and mtime per path, the same baseline rsync gets from its destination."
  >}}
  {{< hextra/feature-card
    title="Batched, not one file at a time"
    subtitle="A thousand small files go up as a handful of xz-compressed packs instead of a thousand round trips."
  >}}
  {{< hextra/feature-card
    title="A resident watch mode"
    subtitle="`dr-sync watch` keeps a container mirrored as files change, debounced and rate-limited so a busy tree behaves."
  >}}
  {{< hextra/feature-card
    title="Ships as a `dr` plugin"
    subtitle="Everything works equally as `dr sync upload ./dir`, or as a standalone binary, or as a container."
  >}}
{{< /hextra/feature-grid >}}
