+++
title = 'datarobot-sync'
layout = 'hextra-home'
+++

{{< hextra/hero-badge link="https://github.com/datarobot-labs" >}}
A DataRobot Solution Labs project
{{< /hextra/hero-badge >}}
{{< hextra/hero-badge link="https://cli.datarobot.com" >}}
Also a DataRobot CLI plugin
{{< /hextra/hero-badge >}}
{{< hextra/hero-badge >}}
Built for always-on agents
{{< /hextra/hero-badge >}}
{{< hextra/hero-badge link="https://www.datarobot.com" >}}
Works with DataRobot 11.4+
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-6">
{{< hextra/hero-headline >}}
  rsync for the&nbsp;<br class="hx:sm:block hx:hidden" />DataRobot Files API
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-subtitle >}}
  An always-on agent that forgets everything on restart isn't always on.&nbsp;<br class="hx:sm:block hx:hidden" />datarobot-sync keeps its skills and memories in <a href="https://www.datarobot.com" target="_blank" rel="noreferrer">DataRobot</a> Files and moves only what changed
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-button text="Read the docs" link="docs/quick-start" >}}
{{< hextra/hero-button text="Run an always-on agent" link="docs/agent-harnesses" style="margin-left:0.75rem" >}}
</div>

<div class="hx:mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Agents keep their memory"
    subtitle="Restore skills, memories, and sessions on boot. A restart, a reschedule, or a new node doesn't wipe what the agent learned."
    link="docs/agent-harnesses"
  >}}
  {{< hextra/feature-card
    title="Roll back a bad write"
    subtitle="Nothing in the container is ever overwritten. Retain a day of generations, and a corrupted memory is a rollback, not a loss."
    link="docs/agent-harnesses"
  >}}
  {{< hextra/feature-card
    title="Exit codes a supervisor can act on"
    subtitle="Authentication failures, lease conflicts, and integrity errors each get their own code, so an entrypoint knows whether to retry, stop, or alert."
    link="docs/exit-codes"
  >}}
  {{< hextra/feature-card
    title="Only the difference moves"
    subtitle="The Files API records a SHA-256 for every stored object, so the diff is exact. Unchanged files never leave the disk."
  >}}
  {{< hextra/feature-card
    title="Renames and reverts cost nothing"
    subtitle="Content is stored under its own digest. Moving a 5 GB file is a manifest edit, not a transfer."
  >}}
  {{< hextra/feature-card
    title="Idle runs stay fast"
    subtitle="The remote manifest records size, mode, and mtime per path, the same baseline rsync gets from its destination."
  >}}
  {{< hextra/feature-card
    title="Packs, not round trips"
    subtitle="A thousand small files go up as a handful of xz-compressed packs instead of a thousand requests."
  >}}
  {{< hextra/feature-card
    title="Mirror as files change"
    subtitle="`dr-sync watch` keeps a container current while a busy tree churns, debounced and rate-limited."
  >}}
  {{< hextra/feature-card
    title="Runs as a DataRobot CLI plugin"
    subtitle="Use `datarobot sync upload ./dir`, a standalone binary, or a container. The behavior is identical."
    link="https://cli.datarobot.com"
  >}}
{{< /hextra/feature-grid >}}
