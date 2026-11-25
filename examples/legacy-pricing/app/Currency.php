<?php

declare(strict_types=1);

namespace UpgradeLab\Pricing;

final class LegacyCurrency
{
    public static function display(int $amount): string
    {
        return sprintf('%d cents', $amount);
    }
}

final class Currency
{
    public static function format(int $amount): string
    {
        return sprintf('%d cents', $amount);
    }
}

function displayAmount(int $amount): string
{
    return LegacyCurrency::display($amount);
}
