import * as React from "react";
import i18next from "i18next";
import {Check, ChevronsUpDown} from "lucide-react";
import {Button} from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import {Popover, PopoverContent, PopoverTrigger} from "@/components/ui/popover";
import {cn} from "@/lib/utils";

export interface SearchableOption {
  value: string;
  label: React.ReactNode;
  /** shown in the dropdown instead of `label`, which stays on the trigger */
  itemLabel?: React.ReactNode;
  keywords?: string;
  disabled?: boolean;
}

interface SearchableSelectProps {
  value?: string | null;
  onChange: (value: string) => void;
  options: SearchableOption[];
  placeholder?: string;
  disabled?: boolean;
  className?: string;
  contentClassName?: string;
  /** keep a value that is not part of options (e.g. loaded from the backend) */
  allowUnknownValue?: boolean;
  /**
   * Server-side search: the parent replaces `options` with the backend matches,
   * so the client-side filter is skipped and every keystroke is reported here.
   */
  onSearch?: (search: string) => void;
  /** scrolling the list to its bottom loads the next page of a remote source */
  onLoadMore?: () => void;
  hasMore?: boolean;
}

/**
 * Single-select combobox with a filter box — the shadcn equivalent of antd's
 * `<Select showSearch />`, used wherever the option list can get long.
 */
export function SearchableSelect({
  value,
  onChange,
  options,
  placeholder,
  disabled,
  className,
  contentClassName,
  allowUnknownValue = true,
  onSearch,
  onLoadMore,
  hasMore = false,
}: SearchableSelectProps) {
  const [open, setOpen] = React.useState(false);
  const [search, setSearch] = React.useState("");

  const handleListScroll = (event: React.SyntheticEvent<HTMLDivElement>) => {
    if (!onLoadMore || !hasMore) {
      return;
    }
    const list = event.currentTarget;
    if (list.scrollHeight - list.scrollTop - list.clientHeight < 48) {
      onLoadMore();
    }
  };

  // `options` may be swapped for a page of search results, but the trigger still
  // has to show the label of the picked value, so remember every label seen
  const labelCache = React.useRef(new Map<string, React.ReactNode>());
  React.useEffect(() => {
    options.forEach((o) => labelCache.current.set(o.value, o.label));
  }, [options]);

  const items = React.useMemo(() => {
    if (allowUnknownValue && value && !options.some((o) => o.value === value)) {
      return [{value, label: labelCache.current.get(value) ?? value}, ...options];
    }
    return options;
  }, [options, value, allowUnknownValue]);

  const selected = items.find((o) => o.value === value);

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) {
          // a closed dropdown starts fresh: the base page, not stale matches
          setSearch("");
          onSearch?.("");
        }
      }}
    >
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          role="combobox"
          disabled={disabled}
          className={cn("w-full justify-between font-normal hover:bg-background", className)}
        >
          <span className={cn("truncate", selected === undefined && "text-muted-foreground")}>
            {selected?.label ?? placeholder ?? ""}
          </span>
          <ChevronsUpDown className="h-4 w-4 shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className={cn("w-[--radix-popover-trigger-width] p-0", contentClassName)} align="start">
        <Command
          filter={
            onSearch
              ? () => 1
              : (itemValue, searchValue) => {
                const option = items.find((o) => o.value === itemValue);
                const haystack = `${itemValue} ${option?.keywords ?? ""} ${
                  typeof option?.label === "string" ? option.label : ""
                }`.toLowerCase();
                return haystack.includes(searchValue.toLowerCase()) ? 1 : 0;
              }
          }
        >
          <CommandInput
            placeholder={i18next.t("general:Search")}
            value={search}
            onValueChange={(v) => {
              setSearch(v);
              onSearch?.(v);
            }}
          />
          <CommandList onScroll={handleListScroll}>
            <CommandEmpty>{i18next.t("general:No data")}</CommandEmpty>
            <CommandGroup>
              {items.map((option) => (
                <CommandItem
                  key={option.value}
                  value={option.value}
                  disabled={option.disabled}
                  onSelect={() => {
                    onChange(option.value);
                    setOpen(false);
                  }}
                >
                  <Check className={cn("mr-2 h-4 w-4", value === option.value ? "opacity-100" : "opacity-0")} />
                  {option.itemLabel ?? option.label}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
