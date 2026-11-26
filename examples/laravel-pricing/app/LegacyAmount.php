<?php

declare(strict_types=1);

namespace PricingLab;

final class LegacyAmount
{
    /** @deprecated Use SaleAmount for nonnegative order amounts. Credit notes still use signed units. */
    public static function normalize(int $units): int
    {
        return $units;
    }
}
