---
title: Maul
template: home.html
hide:
  - navigation
  - toc
---

<!--
  The home page is rendered entirely by overrides/home.html (the landing).
  This body only provides the page title and meta description for the <head>.
-->

Issues in, shipped pull requests out: maul runs sandboxed AI coding agents on your own Kubernetes cluster. An agent
plans each GitHub issue, a human approves the plan, an agent builds it and opens the pull request, and every pull
request gets a live preview. The same engine fixes failing checks on its own pull requests, and investigates security
alerts and fixes the ones it is confident about.
