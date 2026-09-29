# The docs site

The source of https://phoban01.github.io/battery-operator/, built by Jekyll
on GitHub Pages from `main` (`.github/workflows/pages.yml`).

- `index.md` is the home page.
- `install.md` installs a release, with Flux or with `kubectl`, from the
  Manifests' OCI artifact (`docs/requirements/06-deployment.md#manifests-artifact`).
- `hosts.md` makes Hosts on AWS with Cluster API, from the Host Image's AMI
  and the host pools in `config/capi`.
- `demo.cast` is the recording the home page plays. `hack/real-hosts/demo.sh --record`
  records it again, on a Host the real hosts trial brought up
  (`hack/real-hosts/README.md`).

Each page starts with the same line of links to the others. A new page adds
itself to that line on every page.
