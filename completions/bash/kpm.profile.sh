# Load Bash completions for kpm and kpm-index on minimal systems without bash-completion.
if [ -n "${BASH_VERSION:-}" ]; then
	case "$-" in
		*i*)
			if ! complete -p kpm >/dev/null 2>&1 && [ -r /usr/share/bash-completion/completions/kpm ]; then
				. /usr/share/bash-completion/completions/kpm
			fi
			;;
	esac
fi
