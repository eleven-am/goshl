.PHONY: help version bump-major bump-minor bump-patch _do_release

VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)
VERSION := $(or $(VERSION),0.0.0)
MAJOR := $(word 1,$(subst ., ,$(VERSION)))
MINOR := $(word 2,$(subst ., ,$(VERSION)))
PATCH := $(word 3,$(subst ., ,$(VERSION)))

help:
	@echo "Version Management Commands:"
	@echo "  make version      - Show current version"
	@echo "  make bump-major   - Bump major version (X.y.z)"
	@echo "  make bump-minor   - Bump minor version (x.Y.z)"
	@echo "  make bump-patch   - Bump patch version (x.y.Z)"

version:
	@echo "Current version: $(VERSION)"

_do_release:
	@if [ -z "$(NEW_VERSION)" ]; then echo "Error: NEW_VERSION not set"; exit 1; fi; \
	PREV_TAG=$$(git describe --tags --abbrev=0 2>/dev/null || echo ""); \
	DATE=$$(date +%Y-%m-%d); \
	if [ -z "$$PREV_TAG" ]; then \
		COMMITS=$$(git log --oneline --pretty=format:"- %s" 2>/dev/null); \
	else \
		COMMITS=$$(git log $$PREV_TAG..HEAD --oneline --pretty=format:"- %s" 2>/dev/null); \
	fi; \
	if [ -f CHANGELOG.md ]; then \
		mv CHANGELOG.md CHANGELOG.md.bak; \
		echo "# Changelog" > CHANGELOG.md; \
		echo "" >> CHANGELOG.md; \
		echo "## [$(NEW_VERSION)] - $$DATE" >> CHANGELOG.md; \
		echo "" >> CHANGELOG.md; \
		echo "$$COMMITS" >> CHANGELOG.md; \
		echo "" >> CHANGELOG.md; \
		tail -n +2 CHANGELOG.md.bak >> CHANGELOG.md; \
		rm CHANGELOG.md.bak; \
	else \
		echo "# Changelog" > CHANGELOG.md; \
		echo "" >> CHANGELOG.md; \
		echo "## [$(NEW_VERSION)] - $$DATE" >> CHANGELOG.md; \
		echo "" >> CHANGELOG.md; \
		echo "$$COMMITS" >> CHANGELOG.md; \
	fi; \
	git add CHANGELOG.md; \
	git commit -m "🔖 Release v$(NEW_VERSION)"; \
	git tag -a "v$(NEW_VERSION)" -m "Release v$(NEW_VERSION)"; \
	git push origin main; \
	git push origin "v$(NEW_VERSION)"; \
	echo "Version $(NEW_VERSION) released and pushed to origin"

bump-major:
	@NEW_VERSION="$(shell expr $(MAJOR) + 1).0.0"; \
	read -p "Current version is $(VERSION). Bump to $$NEW_VERSION? [y/N] " confirm; \
	if [ "$$confirm" = "y" ] || [ "$$confirm" = "Y" ]; then \
		$(MAKE) _do_release NEW_VERSION=$$NEW_VERSION; \
	else \
		echo "Version bump cancelled"; \
		exit 1; \
	fi

bump-minor:
	@NEW_VERSION="$(MAJOR).$(shell expr $(MINOR) + 1).0"; \
	read -p "Current version is $(VERSION). Bump to $$NEW_VERSION? [y/N] " confirm; \
	if [ "$$confirm" = "y" ] || [ "$$confirm" = "Y" ]; then \
		$(MAKE) _do_release NEW_VERSION=$$NEW_VERSION; \
	else \
		echo "Version bump cancelled"; \
		exit 1; \
	fi

bump-patch:
	@NEW_VERSION="$(MAJOR).$(MINOR).$(shell expr $(PATCH) + 1)"; \
	read -p "Current version is $(VERSION). Bump to $$NEW_VERSION? [y/N] " confirm; \
	if [ "$$confirm" = "y" ] || [ "$$confirm" = "Y" ]; then \
		$(MAKE) _do_release NEW_VERSION=$$NEW_VERSION; \
	else \
		echo "Version bump cancelled"; \
		exit 1; \
	fi
