# The Doblura base image: Odoo, the tools this operator needs, and nothing else.
#
# Doblura used to ship no image at all, on the reasoning that the contract with
# your image is simply "it carries click-odoo-contrib". That reasoning was right
# about the contract and wrong about the cost: meeting it is a Dockerfile most
# people do not want to own, and every image that does not meet it fails in a way
# that is discovered late and read as doblura being broken.
#
# The list below is not a guess. Every line is something that went wrong:
#
#   - The official image runs as uid 100, and Doodba's uid 100 is `messagebus`.
#   - Doodba's published base ships the scaffolding and no Odoo at all.
#   - Doodba's command is `python3`, so `-c odoo.conf` hands the config to the
#     interpreter rather than to Odoo.
#   - An image without click-odoo-contrib cannot restore, update or back up —
#     which is discovered on the day somebody needs a restore.
#
# What this image deliberately does NOT contain: functional modules. Not a
# performance pack, not a curated OCA selection. The image supplies the runtime and
# doblura supplies the configuration — workers, cron threads, the split between web
# and scheduled jobs — which it already sets from `size` and `workload` and can
# change without anybody rebuilding anything. A module baked in here changes what
# the ERP does in a layer the customer did not choose and cannot see. Modules
# belong in spec.addons.repos, where they are named, pinned to a commit, and
# visible on the screen. See DECISIONS.md 11.
#
#   docker build --build-arg ODOO_VERSION=19.0 -t doblura/odoo:19.0 images/odoo
ARG ODOO_VERSION=19.0
FROM odoo:${ODOO_VERSION}

ARG ODOO_VERSION
LABEL org.opencontainers.image.title="Doblura base image for Odoo ${ODOO_VERSION}" \
      org.opencontainers.image.description="Odoo Community with the tools the Doblura operator needs. Not an official Odoo build." \
      org.opencontainers.image.source="https://github.com/doblura/doblura" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      dev.doblura.odoo-version="${ODOO_VERSION}" \
      dev.doblura.edition="community"

USER root

# click-odoo-contrib, and a build that fails if its entry points are ever renamed.
#
# --break-system-packages: the Odoo images are Debian with a PEP 668 managed
# Python, and click-odoo has to import odoo itself, so it goes in the system
# interpreter on purpose.
#
# The --help calls are the point of this layer. It is click-odoo-backupDB, not
# click-odoo-backup — a detail only a real run surfaces, and one this build now
# surfaces instead.
RUN pip3 install --no-cache-dir --break-system-packages click-odoo-contrib \
 && for c in click-odoo click-odoo-backupdb click-odoo-restoredb \
             click-odoo-update click-odoo-dropdb click-odoo-copydb; do \
      "$c" --help >/dev/null || { echo "MISSING: $c"; exit 1; }; \
    done

# Everything below is a check, not an install. The image is only useful if these
# hold, and a build that fails is enormously cheaper than an environment that
# comes up and cannot be restored.
RUN set -eu; \
    # Odoo is really in here, and it is the version the tag claims. Read from
    # Odoo itself rather than from the tag: a tag is something somebody typed.
    real=$(python3 -c 'import odoo, sys; sys.stdout.write(odoo.release.version)'); \
    case "$real" in "${ODOO_VERSION%%.*}"*) ;; \
      *) echo "the image says ${ODOO_VERSION} and Odoo says $real"; exit 1;; esac; \
    # wkhtmltopdf, which is the oldest paper cut in this ecosystem: Odoo needs a
    # patched-qt build, and the unpatched one silently produces broken headers and
    # footers on every PDF a customer sends out.
    wkhtmltopdf --version >/dev/null || { echo "no wkhtmltopdf"; exit 1; }; \
    wkhtmltopdf --version 2>&1 | grep -qi "with patched qt" \
      || echo "WARNING: wkhtmltopdf is not the patched-qt build; PDF headers and footers will be wrong"; \
    # The database client tools, which every restore and backup shells out to.
    psql --version >/dev/null || { echo "no psql"; exit 1; }; \
    pg_dump --version >/dev/null || { echo "no pg_dump"; exit 1; }; \
    echo "odoo $real, click-odoo-contrib, wkhtmltopdf and the postgres client are all present"

# Back to the unprivileged user the official image defines. Doblura's Official
# flavour expects exactly this: user odoo, command `odoo`, entrypoint from the
# base image.
USER odoo
