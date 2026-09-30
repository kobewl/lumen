.PHONY: run test check secrets-check
run:
	python3 -m lumen
test:
	python3 -m unittest discover -s tests -v
secrets-check:
	python3 scripts/check_secrets.py
check: test secrets-check
	python3 -m compileall -q lumen tests
