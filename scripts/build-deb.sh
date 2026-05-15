#!/usr/bin/env bash
# build-deb.sh — assembles a production .deb package for openstack-network-manager
set -euo pipefail

VERSION="${1:-1.0.0}"
ARCH="amd64"
PKG="openstack-network-manager"
BUILD_DIR="dist/deb-build"
DEB_FILE="dist/${PKG}_${VERSION}_${ARCH}.deb"

echo "--> Cleaning previous build"
rm -rf "${BUILD_DIR}"

echo "--> Creating package layout"
install -d "${BUILD_DIR}/DEBIAN"
install -d "${BUILD_DIR}/usr/bin"
install -d "${BUILD_DIR}/etc/openstack-network-manager"
install -d "${BUILD_DIR}/lib/systemd/system"

echo "--> Copying files"
install -m 0755 "dist/${PKG}"                                   "${BUILD_DIR}/usr/bin/${PKG}"
install -m 0644 "systemd/${PKG}.service"                        "${BUILD_DIR}/lib/systemd/system/${PKG}.service"
install -m 0644 "config/config.yaml.default"                    "${BUILD_DIR}/etc/openstack-network-manager/config.yaml"

echo "--> Copying DEBIAN control files"
install -m 0644 packaging/DEBIAN/control                        "${BUILD_DIR}/DEBIAN/control"
install -m 0644 packaging/DEBIAN/conffiles                      "${BUILD_DIR}/DEBIAN/conffiles"
install -m 0755 packaging/DEBIAN/postinst                       "${BUILD_DIR}/DEBIAN/postinst"
install -m 0755 packaging/DEBIAN/prerm                          "${BUILD_DIR}/DEBIAN/prerm"
install -m 0755 packaging/DEBIAN/postrm                         "${BUILD_DIR}/DEBIAN/postrm"

echo "--> Updating version in control file"
sed -i "s/^Version:.*/Version: ${VERSION}/" "${BUILD_DIR}/DEBIAN/control"

echo "--> Calculating installed size"
SIZE_KB=$(du -sk "${BUILD_DIR}" | awk '{print $1}')
echo "Installed-Size: ${SIZE_KB}" >> "${BUILD_DIR}/DEBIAN/control"

echo "--> Building .deb"
dpkg-deb --build --root-owner-group "${BUILD_DIR}" "${DEB_FILE}"

echo ""
echo "==> Package ready: ${DEB_FILE}"
echo ""
echo "Install on target Ubuntu machine:"
echo "  sudo dpkg -i ${DEB_FILE}"
echo ""
echo "Verify service:"
echo "  sudo systemctl status openstack-network-manager"
echo "  sudo journalctl -u openstack-network-manager -f"
